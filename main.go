// wavepos is a tiny fiat point-of-sale in front of a Wavelength (waved)
// daemon: punch an amount, get a Lightning invoice QR, see it go green.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
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

const (
	// maxCents caps a single bill (in fiat cents) so a fat-fingered amount
	// can't create a silly invoice.
	maxCents = 100000 // 1,000.00

	// qrLife is how long a QR is shown before the till offers a fresh one.
	qrLife = 15 * time.Minute

	// watchLife is how long every invoice is watched for payment, including
	// replaced, timed-out and cancelled ones. It outlives the invoice expiry
	// so a late payment is always noticed.
	watchLife = 65 * time.Minute

	// keepLife is how long a finished bill stays queryable.
	keepLife = 2 * time.Hour
)

// zeroDecimal lists currencies without cents. The keypad works in cents, so
// they aren't supported.
var zeroDecimal = map[string]bool{
	"JPY": true, "KRW": true, "VND": true, "CLP": true, "ISK": true,
	"PYG": true, "UGX": true, "XAF": true, "XOF": true, "IDR": true,
}

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

// satsFor converts fiat cents to sats at price (fiat per BTC).
func satsFor(cents int64, price float64) int64 {
	return int64(math.Round(float64(cents) / 100 / price * 1e8))
}

// splitCents divides a bill into near-equal parts that each stay under
// limitSats at price, so no single invoice passes the operator's per-VTXO
// maximum.
func splitCents(total int64, price float64, limitSats int64) []int64 {
	n := int64(1)
	if s := satsFor(total, price); s > limitSats {
		n = (s + limitSats - 1) / limitSats
	}
	parts := make([]int64, n)
	for i := range parts {
		parts[i] = total / n
		if int64(i) < total%n {
			parts[i]++
		}
	}
	return parts
}

// fmtCents renders cents as "12.50 AUD".
func fmtCents(c int64, cur string) string {
	return fmt.Sprintf("%d.%02d %s", c/100, c%100, cur)
}

// invoice is one Lightning invoice created for a part of a bill.
type invoice struct {
	id      string // waved WalletEntry id
	bolt11  string
	qr      string // PNG data URL
	part    int
	cents   int64
	sats    int64
	created time.Time
	paid    bool
	failed  bool
}

// watched reports whether the invoice can still be paid and needs checking.
func (in *invoice) watched() bool {
	return !in.paid && !in.failed && time.Since(in.created) < watchLife
}

// bill is one sale. The server owns its state so that a reload, a cancel or
// a retry can never lose track of a payment.
type bill struct {
	id       string
	total    int64   // cents
	parts    []int64 // cents per part
	paid     []bool  // per part
	cur      int     // first unpaid part
	invoices []*invoice
	state    string // open, paid, cancelled
	paidC    int64  // cents of distinct paid parts
	extraC   int64  // cents paid twice for the same part
	err      string // last invoice creation error
	creating bool   // an invoice request is in flight
	touched  time.Time
}

// current returns the newest invoice for the current part, if any.
func (b *bill) current() *invoice {
	for i := len(b.invoices) - 1; i >= 0; i-- {
		if b.invoices[i].part == b.cur {
			return b.invoices[i]
		}
	}
	return nil
}

type notice struct {
	ID   int    `json:"id"`
	Text string `json:"text"`
}

type server struct {
	wave       *waveClient
	rate       *rateCache
	memo       string
	maxInvoice int64 // operator max_vtxo_amount: largest single receive
	maxBalance int64 // operator max_user_balance: most the wallet may hold

	mu      sync.Mutex
	bills   map[string]*bill
	notices []notice
	nextNID int
}

// addNotice records something staff must act on, such as an overpayment.
// Callers hold s.mu.
func (s *server) addNotice(format string, args ...any) {
	s.nextNID++
	n := notice{ID: s.nextNID, Text: fmt.Sprintf(format, args...)}
	s.notices = append(s.notices, n)
	log.Printf("NOTICE: %s", n.Text)
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

// outstanding sums the sats of every invoice that could still be paid,
// except those for skip's current part (about to be replaced). Callers hold
// s.mu.
func (s *server) outstanding(skip *bill) int64 {
	var sum int64
	for _, b := range s.bills {
		for _, in := range b.invoices {
			if in.watched() && !(b == skip && in.part == skip.cur) {
				sum += in.sats
			}
		}
	}
	return sum
}

// status asks waved whether an invoice is pending, paid or failed.
func (s *server) status(ctx context.Context, id string) (string, error) {
	var res struct {
		Entry struct {
			Status json.RawMessage `json:"status"`
		} `json:"entry"`
	}
	err := s.wave.call(ctx, "/v1/wallet/inspect/activity",
		map[string]any{"id": id}, &res)
	if err != nil {
		return "", err
	}
	// The gateway may render the enum as its name or its number.
	switch strings.Trim(string(res.Entry.Status), `"`) {
	case "ENTRY_STATUS_COMPLETE", "2":
		return "paid", nil
	case "ENTRY_STATUS_FAILED", "3":
		return "failed", nil
	}
	return "pending", nil
}

// newInvoice creates the invoice for b's current part. A second call while
// one is in flight does nothing, so a double-tapped Retry is harmless.
func (s *server) newInvoice(ctx context.Context, b *bill) {
	s.mu.Lock()
	if b.creating || b.state != "open" || b.cur >= len(b.parts) {
		s.mu.Unlock()
		return
	}
	b.creating = true
	part, cents := b.cur, b.parts[b.cur]
	var remaining int64
	for _, c := range b.parts[b.cur:] {
		remaining += c
	}
	other := s.outstanding(b)
	s.mu.Unlock()

	in, err := s.makeInvoice(ctx, part, cents, remaining, other)

	s.mu.Lock()
	defer s.mu.Unlock()
	b.creating = false
	b.touched = time.Now()
	if err != nil {
		b.err = err.Error()
		log.Printf("bill %s part %d: %v", b.id, part+1, err)
		return
	}
	b.err = ""
	// Keep it even if the bill was cancelled meanwhile: it can still be
	// paid, so it must be watched.
	b.invoices = append(b.invoices, in)
}

func (s *server) makeInvoice(ctx context.Context, part int, cents, remaining,
	other int64) (*invoice, error) {

	cur := s.rate.currency
	price, err := s.rate.get(ctx)
	if err != nil {
		return nil, fmt.Errorf("no BTC/%s rate: %w", cur, err)
	}
	sats := satsFor(cents, price)
	if sats < 1 {
		return nil, errors.New("amount too small")
	}
	if sats > s.maxInvoice {
		return nil, fmt.Errorf("the BTC price moved since this bill was split; "+
			"cancel and charge the remaining %s again",
			fmtCents(remaining, cur))
	}
	held, err := s.held(ctx)
	if err != nil {
		return nil, err
	}
	if room := s.maxBalance - held - other; satsFor(remaining, price) > room {
		roomC := int64(float64(max(room, 0)) / 1e8 * price * 100)
		return nil, fmt.Errorf("bill too big for the wallet right now: max %s "+
			"(a sweep frees up room every few minutes)", fmtCents(roomC, cur))
	}

	amount := fmtCents(cents, cur)
	var recv struct {
		Invoice string `json:"invoice"`
		Entry   struct {
			ID string `json:"id"`
		} `json:"entry"`
	}
	err = s.wave.call(ctx, "/v1/wallet/recv", map[string]any{
		"amt_sat": sats,
		"memo":    strings.TrimSpace(s.memo + " " + amount),
	}, &recv)
	if err != nil {
		return nil, err
	}
	if recv.Invoice == "" || recv.Entry.ID == "" {
		return nil, errors.New("waved returned no invoice")
	}

	// Uppercase keeps the QR in alphanumeric mode: smaller, easier to scan.
	png, err := qrcode.Encode(strings.ToUpper("lightning:"+recv.Invoice),
		qrcode.Medium, 640)
	if err != nil {
		return nil, err
	}
	log.Printf("invoice %s: %s = %d sats @ %.0f", recv.Entry.ID, amount, sats, price)
	return &invoice{
		id: recv.Entry.ID, bolt11: recv.Invoice, part: part, cents: cents,
		sats: sats, created: time.Now(),
		qr: "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	}, nil
}

// settle applies a status result to an invoice and its bill. Callers hold
// s.mu.
func (s *server) settle(b *bill, in *invoice, status string) {
	cur := s.rate.currency
	switch {
	case status == "failed":
		in.failed = true
	case status == "paid" && !in.paid:
		in.paid = true
		b.touched = time.Now()
		if b.paid[in.part] {
			b.extraC += in.cents
			s.addNotice("Bill %s: payment %d was paid twice. The customer "+
				"overpaid %s, refund them.", b.id, in.part+1, fmtCents(in.cents, cur))
			return
		}
		b.paid[in.part] = true
		b.paidC += in.cents
		if b.state == "cancelled" {
			s.addNotice("Bill %s was cancelled, but %s was paid on it "+
				"afterwards. Refund the customer or count it towards their bill.",
				b.id, fmtCents(in.cents, cur))
		}
		for b.cur < len(b.parts) && b.paid[b.cur] {
			b.cur++
		}
		if b.state == "open" && b.cur == len(b.parts) {
			b.state = "paid"
		}
	}
}

// watch checks every invoice of a bill, one request at a time, until none
// can be paid any more, and moves the bill on to its next part.
func (s *server) watch(b *bill) {
	for {
		time.Sleep(2 * time.Second)

		s.mu.Lock()
		var todo []*invoice
		for _, in := range b.invoices {
			if in.watched() {
				todo = append(todo, in)
			}
		}
		if b.state == "open" && len(todo) == 0 && !b.creating &&
			time.Since(b.touched) > watchLife {
			b.state = "cancelled" // abandoned without Cancel
		}
		done := b.state != "open" && len(todo) == 0 && !b.creating
		s.mu.Unlock()

		if done {
			time.AfterFunc(keepLife, func() {
				s.mu.Lock()
				delete(s.bills, b.id)
				s.mu.Unlock()
			})
			return
		}

		for _, in := range todo {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			st, err := s.status(ctx, in.id)
			cancel()
			if err != nil {
				log.Printf("status %s: %v", in.id, err)
				continue
			}
			s.mu.Lock()
			s.settle(b, in, st)
			s.mu.Unlock()
		}

		// After a part is paid, line up the next one.
		s.mu.Lock()
		next := b.state == "open" && b.cur < len(b.parts) &&
			b.current() == nil && b.err == "" && !b.creating
		s.mu.Unlock()
		if next {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			s.newInvoice(ctx, b)
			cancel()
		}
	}
}

// billView is what the till sees.
type billView struct {
	ID         string   `json:"id"`
	State      string   `json:"state"`
	TotalCents int64    `json:"total_cents"`
	PaidCents  int64    `json:"paid_cents"`
	ExtraCents int64    `json:"extra_cents"`
	Part       int      `json:"part"`  // 1-based current part
	Parts      int      `json:"parts"` // number of parts
	Error      string   `json:"error,omitempty"`
	Current    *qrView  `json:"current,omitempty"`
	CanRetry   bool     `json:"can_retry"`
	Notices    []notice `json:"notices"`
}

type qrView struct {
	Cents   int64  `json:"cents"`
	Sats    int64  `json:"sats"`
	Invoice string `json:"invoice"`
	QR      string `json:"qr"`
}

// view snapshots a bill for the till. Callers hold s.mu.
func (s *server) view(b *bill) billView {
	v := billView{
		ID: b.id, State: b.state, TotalCents: b.total, PaidCents: b.paidC,
		ExtraCents: b.extraC, Part: min(b.cur+1, len(b.parts)),
		Parts: len(b.parts), Error: b.err, Notices: s.notices,
	}
	if b.state != "open" {
		return v
	}
	in := b.current()
	fresh := in != nil && !in.failed && time.Since(in.created) < qrLife
	if fresh && b.err == "" {
		v.Current = &qrView{Cents: in.cents, Sats: in.sats, Invoice: in.bolt11, QR: in.qr}
	}
	v.CanRetry = !b.creating && (!fresh || b.err != "")
	return v
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
	s.mu.Lock()
	notices := s.notices
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"btc_price":        p,
		"currency":         s.rate.currency,
		"max_invoice_sats": s.maxInvoice,
		"notices":          notices,
	})
}

// handleNewBill starts a sale: split it, create the first invoice and start
// watching.
func (s *server) handleNewBill(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Cents int64 `json:"cents"`
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
	price, err := s.rate.get(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway,
			fmt.Errorf("no BTC/%s rate: %w", s.rate.currency, err))
		return
	}

	// Each part stays 10% under the limit so a small price move between
	// parts can't push one over.
	parts := splitCents(req.Cents, price, s.maxInvoice*9/10)
	id := make([]byte, 4)
	rand.Read(id)
	b := &bill{
		id: hex.EncodeToString(id), total: req.Cents, parts: parts,
		paid: make([]bool, len(parts)), state: "open", touched: time.Now(),
	}
	s.newInvoice(r.Context(), b)

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(b.invoices) == 0 {
		// Nothing was created, so there's nothing to watch: refuse outright.
		writeErr(w, http.StatusConflict, errors.New(b.err))
		return
	}
	s.bills[b.id] = b
	go s.watch(b)
	writeJSON(w, http.StatusOK, s.view(b))
}

// lookup finds the bill named by ?id=. Callers hold s.mu.
func (s *server) lookup(w http.ResponseWriter, r *http.Request) *bill {
	b := s.bills[r.URL.Query().Get("id")]
	if b == nil {
		writeErr(w, http.StatusNotFound, errors.New("no such bill"))
	}
	return b
}

func (s *server) handleBill(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.lookup(w, r); b != nil {
		writeJSON(w, http.StatusOK, s.view(b))
	}
}

// handleRetry makes a fresh QR for the current part. It's a no-op while the
// current QR is still fresh, so tapping twice can't create extra invoices.
func (s *server) handleRetry(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	b := s.lookup(w, r)
	if b == nil {
		s.mu.Unlock()
		return
	}
	retry := s.view(b).CanRetry
	if retry {
		b.err = ""
	}
	s.mu.Unlock()

	if retry {
		s.newInvoice(r.Context(), b)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, http.StatusOK, s.view(b))
}

// handleCancel stops a bill. Its invoices are still watched, so a payment
// that arrives afterwards raises a notice instead of going unnoticed.
func (s *server) handleCancel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.lookup(w, r)
	if b == nil {
		return
	}
	if b.state == "open" {
		b.state = "cancelled"
		log.Printf("bill %s cancelled with %s paid", b.id, fmtCents(b.paidC, s.rate.currency))
	}
	writeJSON(w, http.StatusOK, s.view(b))
}

// handleAck dismisses a notice once staff have dealt with it.
func (s *server) handleAck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.notices[:0:0]
	for _, n := range s.notices {
		if n.ID != id {
			kept = append(kept, n)
		}
	}
	s.notices = kept
	writeJSON(w, http.StatusOK, map[string]any{"notices": s.notices})
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
		"fiat currency code with cents, e.g. AUD, USD, EUR")
	memo := flag.String("memo", "", "invoice memo prefix, e.g. your shop name")
	maxInvoice := flag.Int64("max-invoice-sats", 50000,
		"largest single invoice (operator max_vtxo_amount)")
	maxBalance := flag.Int64("max-balance-sats", 300000,
		"most the wallet may hold (operator max_user_balance)")
	keyFile := flag.String("key-file", "",
		"file holding the shop key; empty leaves the POS unlocked")
	icon := flag.String("icon", "",
		"PNG shown as the browser tab and home-screen icon (180x180 works best)")
	flag.Parse()

	cur := strings.ToUpper(*currency)
	if zeroDecimal[cur] {
		log.Fatalf("-currency %s has no cents, which WavePos doesn't support yet", cur)
	}
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
		rate: &rateCache{currency: cur,
			hc: &http.Client{Timeout: 10 * time.Second}},
		memo:       *memo,
		maxInvoice: *maxInvoice,
		maxBalance: *maxBalance,
		bills:      make(map[string]*bill),
	}

	mux := http.NewServeMux()
	mux.Handle("/", http.FileServer(http.FS(static)))
	mux.HandleFunc("/api/rate", s.handleRate)
	mux.HandleFunc("/api/bill/new", s.handleNewBill)
	mux.HandleFunc("/api/bill", s.handleBill)
	mux.HandleFunc("/api/bill/retry", s.handleRetry)
	mux.HandleFunc("/api/bill/cancel", s.handleCancel)
	mux.HandleFunc("/api/notice/ack", s.handleAck)
	mux.HandleFunc("/icon.png", func(w http.ResponseWriter, r *http.Request) {
		if *icon == "" {
			http.NotFound(w, r)
			return
		}
		http.ServeFile(w, r, *icon)
	})

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
		*listen, *waveURL, cur, *keyFile != "")
	log.Fatal(http.ListenAndServe(*listen, handler))
}
