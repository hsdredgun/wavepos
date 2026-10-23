// wavepos is a tiny fiat point-of-sale in front of a Wavelength (waved)
// daemon: punch an amount, get a Lightning invoice QR, see it go green.
package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

//go:embed index.html
var static embed.FS

// maxCents caps a single sale (in fiat cents) so a fat-fingered amount can't
// create a silly invoice. Bills above the per-invoice limit are split by the
// frontend.
const maxCents = 100000 // 1,000.00

// waveClient talks to the waved REST gateway.
type waveClient struct {
	base string
	mac  string
	hc   *http.Client
}

func (w *waveClient) call(ctx context.Context, path string, req, resp any) error {
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, w.base+path,
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("macaroon", w.mac)

	res, err := w.hc.Do(r)
	if err != nil {
		return fmt.Errorf("waved unreachable: %w", err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		var e struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(data, &e) == nil && e.Message != "" {
			return errors.New(e.Message)
		}
		return fmt.Errorf("waved: HTTP %d", res.StatusCode)
	}
	return json.Unmarshal(data, resp)
}

// rateCache holds the BTC price in the configured fiat currency, refreshed at
// most once a minute.
type rateCache struct {
	mu       sync.Mutex
	currency string
	price    float64
	at       time.Time
	hc       *http.Client
}

func (c *rateCache) get(ctx context.Context) (float64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.price > 0 && time.Since(c.at) < time.Minute {
		return c.price, nil
	}
	p, err := c.coinbase(ctx)
	if err != nil {
		log.Printf("coinbase rate: %v, trying kraken", err)
		p, err = c.kraken(ctx)
	}
	if err != nil {
		// A slightly stale price beats refusing the sale.
		if c.price > 0 && time.Since(c.at) < 10*time.Minute {
			return c.price, nil
		}
		return 0, err
	}
	c.price, c.at = p, time.Now()
	return p, nil
}

func (c *rateCache) fetch(ctx context.Context, url string, v any) error {
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := c.hc.Do(r)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(v)
}

func (c *rateCache) coinbase(ctx context.Context) (float64, error) {
	var v struct {
		Data struct {
			Amount string `json:"amount"`
		} `json:"data"`
	}
	url := "https://api.coinbase.com/v2/prices/BTC-" + c.currency + "/spot"
	if err := c.fetch(ctx, url, &v); err != nil {
		return 0, err
	}
	return parsePrice(v.Data.Amount)
}

func (c *rateCache) kraken(ctx context.Context) (float64, error) {
	var v struct {
		Error  []string `json:"error"`
		Result map[string]struct {
			C []string `json:"c"`
		} `json:"result"`
	}
	url := "https://api.kraken.com/0/public/Ticker?pair=XBT" + c.currency
	if err := c.fetch(ctx, url, &v); err != nil {
		return 0, err
	}
	for _, t := range v.Result {
		if len(t.C) > 0 {
			return parsePrice(t.C[0])
		}
	}
	return 0, fmt.Errorf("kraken: no price %v", v.Error)
}

func parsePrice(s string) (float64, error) {
	p, err := strconv.ParseFloat(s, 64)
	if err != nil || p <= 0 || math.IsInf(p, 0) {
		return 0, fmt.Errorf("bad price %q", s)
	}
	return p, nil
}

type server struct {
	wave       *waveClient
	rate       *rateCache
	memo       string
	maxInvoice int64 // operator max_vtxo_amount: largest single receive
	maxBalance int64 // operator max_user_balance: most the wallet may hold
}

// held returns what the wallet holds or is about to hold, for the
// max_user_balance check.
func (s *server) held(ctx context.Context) (int64, error) {
	var st struct {
		Balance struct {
			Confirmed int64 `json:"confirmed_sat,string"`
			PendingIn int64 `json:"pending_in_sat,string"`
			Credit    int64 `json:"credit_available_sat,string"`
		} `json:"balance"`
	}
	if err := s.wave.call(ctx, "/v1/wallet/status", map[string]any{}, &st); err != nil {
		return 0, err
	}
	b := st.Balance
	return b.Confirmed + b.PendingIn + b.Credit, nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	log.Printf("error: %v", err)
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *server) handleRate(w http.ResponseWriter, r *http.Request) {
	p, err := s.rate.get(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"btc_price":        p,
		"currency":         s.rate.currency,
		"max_invoice_sats": s.maxInvoice,
	})
}

func (s *server) handleInvoice(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Cents int64 `json:"cents"`
		// RemainingCents is this part plus every unpaid part after it, so a
		// split bill is refused up front rather than half-way through.
		RemainingCents int64 `json:"remaining_cents"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1024)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.Cents <= 0 || req.Cents > maxCents {
		writeErr(w, http.StatusBadRequest,
			fmt.Errorf("amount must be between 0.01 and %d", maxCents/100))
		return
	}
	if req.RemainingCents < req.Cents {
		req.RemainingCents = req.Cents
	}

	cur := s.rate.currency
	price, err := s.rate.get(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("no BTC/%s rate: %w", cur, err))
		return
	}
	sats := int64(math.Round(float64(req.Cents) / 100 / price * 1e8))
	if sats < 1 {
		writeErr(w, http.StatusBadRequest, errors.New("amount too small"))
		return
	}
	if sats > s.maxInvoice {
		writeErr(w, http.StatusBadRequest, fmt.Errorf(
			"%d sats is over the %d sat per-payment limit, split the bill",
			sats, s.maxInvoice))
		return
	}
	held, err := s.held(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	remainingSats := int64(math.Round(float64(req.RemainingCents) / 100 / price * 1e8))
	if held+remainingSats > s.maxBalance {
		room := max(s.maxBalance-held, 0)
		roomCents := int64(float64(room) / 1e8 * price * 100)
		writeErr(w, http.StatusConflict, fmt.Errorf(
			"bill too big for the wallet right now: max %d.%02d %s "+
				"(a sweep frees up room every few minutes)",
			roomCents/100, roomCents%100, cur))
		return
	}

	amount := fmt.Sprintf("%d.%02d %s", req.Cents/100, req.Cents%100, cur)
	var recv struct {
		Invoice string `json:"invoice"`
		Entry   struct {
			ID string `json:"id"`
		} `json:"entry"`
	}
	err = s.wave.call(r.Context(), "/v1/wallet/recv", map[string]any{
		"amt_sat": sats,
		"memo":    strings.TrimSpace(s.memo + " " + amount),
	}, &recv)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if recv.Invoice == "" || recv.Entry.ID == "" {
		writeErr(w, http.StatusBadGateway, errors.New("waved returned no invoice"))
		return
	}

	// Uppercase keeps the QR in alphanumeric mode: smaller, easier to scan.
	png, err := qrcode.Encode(strings.ToUpper("lightning:"+recv.Invoice),
		qrcode.Medium, 640)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	log.Printf("invoice %s: %s = %d sats @ %.0f", recv.Entry.ID, amount, sats, price)
	writeJSON(w, http.StatusOK, map[string]any{
		"id":      recv.Entry.ID,
		"invoice": recv.Invoice,
		"sats":    sats,
		"qr":      "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("missing id"))
		return
	}
	var res struct {
		Entry struct {
			Status json.RawMessage `json:"status"`
		} `json:"entry"`
	}
	err := s.wave.call(r.Context(), "/v1/wallet/inspect/activity",
		map[string]any{"id": id}, &res)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	// The gateway may render the enum as its name or its number.
	status := "pending"
	switch strings.Trim(string(res.Entry.Status), `"`) {
	case "ENTRY_STATUS_COMPLETE", "2":
		status = "paid"
	case "ENTRY_STATUS_FAILED", "3":
		status = "failed"
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

// requireKey locks the POS behind a shared shop key. Visiting a page with
// ?k=<key> stores the key in a cookie for the API calls that follow, so the
// till only needs a bookmarked link and no account. The key stays in the URL
// on purpose: an iOS home-screen app keeps its own cookie jar and bookmarks
// the URL as shown.
func requireKey(key string, next http.Handler) http.Handler {
	const cookie = "wavepos_key"
	match := func(s string) bool {
		return subtle.ConstantTimeCompare([]byte(s), []byte(key)) == 1
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if k := r.URL.Query().Get("k"); k != "" && match(k) {
			http.SetCookie(w, &http.Cookie{
				Name: cookie, Value: key, Path: "/", MaxAge: 365 * 24 * 3600,
				HttpOnly: true, SameSite: http.SameSiteStrictMode,
			})
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(cookie); err == nil && match(c.Value) {
			next.ServeHTTP(w, r)
			return
		}
		http.Error(w, "Locked", http.StatusUnauthorized)
	})
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8080", "HTTP listen address")
	waveURL := flag.String("waved", "http://127.0.0.1:10031", "waved REST gateway URL")
	macPath := flag.String("macaroon", "",
		"waved macaroon file (bake a receive-only one, see README)")
	currency := flag.String("currency", "AUD",
		"fiat currency code for the keypad, e.g. AUD, USD, EUR")
	memo := flag.String("memo", "", "invoice memo prefix, e.g. your shop name")
	maxInvoice := flag.Int64("max-invoice-sats", 50000,
		"largest single invoice (operator max_vtxo_amount)")
	maxBalance := flag.Int64("max-balance-sats", 300000,
		"most the wallet may hold (operator max_user_balance)")
	keyFile := flag.String("key-file", "",
		"file holding the shop key; empty leaves the POS unlocked")
	flag.Parse()

	if *macPath == "" {
		log.Fatal("-macaroon is required")
	}
	mac, err := os.ReadFile(*macPath)
	if err != nil {
		log.Fatalf("read macaroon: %v", err)
	}

	hc := &http.Client{Timeout: 30 * time.Second}
	s := &server{
		wave: &waveClient{base: strings.TrimRight(*waveURL, "/"),
			mac: hex.EncodeToString(mac), hc: hc},
		rate: &rateCache{currency: strings.ToUpper(*currency),
			hc: &http.Client{Timeout: 10 * time.Second}},
		memo:       *memo,
		maxInvoice: *maxInvoice,
		maxBalance: *maxBalance,
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/rate", s.handleRate)
	mux.HandleFunc("/api/invoice", s.handleInvoice)
	mux.HandleFunc("/api/status", s.handleStatus)

	var handler http.Handler = mux
	if *keyFile != "" {
		key, err := os.ReadFile(*keyFile)
		if err != nil {
			log.Fatalf("read key: %v", err)
		}
		k := strings.TrimSpace(string(key))
		if len(k) < 16 {
			log.Fatalf("shop key in %s is shorter than 16 chars", *keyFile)
		}
		handler = requireKey(k, mux)
	}

	log.Printf("wavepos listening on %s (waved %s, %s, locked=%v)",
		*listen, *waveURL, s.rate.currency, *keyFile != "")
	log.Fatal(http.ListenAndServe(*listen, handler))
}
