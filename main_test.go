package main

import "testing"

func TestSatsFor(t *testing.T) {
	tests := []struct {
		cents int64
		price float64
		want  int64
	}{
		{100, 100000, 1000},        // 1.00 at 100k/BTC
		{25, 116279, 215},          // the first real test sale
		{1, 1_000_000_000, 0},      // rounds down to nothing
		{100000, 50000, 2_000_000}, // 1,000.00 at 50k/BTC
	}
	for _, tt := range tests {
		if got := satsFor(tt.cents, tt.price); got != tt.want {
			t.Errorf("satsFor(%d, %v) = %d, want %d", tt.cents, tt.price, got, tt.want)
		}
	}
}

func TestSplitCents(t *testing.T) {
	const price = 116279 // fiat per BTC
	const limit = 45000  // sats per part

	tests := []struct {
		total     int64
		wantParts int
	}{
		{25, 1},     // tiny
		{5000, 1},   // ~43k sats fits in one
		{15000, 3},  // ~129k sats
		{20000, 4},  // ~172k sats
		{100000, 20}, // the max bill
	}
	for _, tt := range tests {
		parts := splitCents(tt.total, price, limit)
		if len(parts) != tt.wantParts {
			t.Errorf("splitCents(%d): %d parts, want %d", tt.total, len(parts), tt.wantParts)
		}
		var sum int64
		for i, p := range parts {
			sum += p
			if s := satsFor(p, price); s > limit {
				t.Errorf("splitCents(%d): part %d is %d sats, over %d", tt.total, i, s, limit)
			}
			if d := p - parts[0]; d < -1 || d > 1 {
				t.Errorf("splitCents(%d): parts %v aren't equal to the cent", tt.total, parts)
			}
		}
		if sum != tt.total {
			t.Errorf("splitCents(%d): parts sum to %d", tt.total, sum)
		}
	}
}

func TestSettle(t *testing.T) {
	s := &server{rate: &rateCache{currency: "AUD"}, bills: map[string]*bill{}}
	b := &bill{id: "b", total: 300, parts: []int64{100, 100, 100},
		paid: make([]bool, 3), state: "open"}
	first := &invoice{part: 0, cents: 100}
	retried := &invoice{part: 0, cents: 100}
	b.invoices = []*invoice{first, retried}

	// Paying the old and the retried QR for the same part counts once and
	// flags the overpayment.
	s.settle(b, first, "paid")
	s.settle(b, retried, "paid")
	if b.paidC != 100 || b.extraC != 100 || b.cur != 1 {
		t.Fatalf("after double pay: paid %d extra %d cur %d", b.paidC, b.extraC, b.cur)
	}
	if len(s.notices) != 1 {
		t.Fatalf("want 1 overpay notice, got %v", s.notices)
	}
	// A repeated "paid" status for the same invoice changes nothing.
	s.settle(b, first, "paid")
	if b.paidC != 100 || b.extraC != 100 {
		t.Fatalf("repeat status counted again: paid %d extra %d", b.paidC, b.extraC)
	}

	// Paying after cancel is recorded with a notice; the bill stays cancelled.
	b.state = "cancelled"
	late := &invoice{part: 1, cents: 100}
	b.invoices = append(b.invoices, late)
	s.settle(b, late, "paid")
	if b.paidC != 200 || b.state != "cancelled" || len(s.notices) != 2 {
		t.Fatalf("late pay: paid %d state %s notices %v", b.paidC, b.state, s.notices)
	}

	// Paying every part of an open bill closes it.
	b2 := &bill{id: "c", parts: []int64{50, 50}, paid: make([]bool, 2), state: "open"}
	for i := range b2.parts {
		in := &invoice{part: i, cents: 50}
		b2.invoices = append(b2.invoices, in)
		s.settle(b2, in, "paid")
	}
	if b2.state != "paid" || b2.paidC != 100 {
		t.Fatalf("full pay: state %s paid %d", b2.state, b2.paidC)
	}
}
