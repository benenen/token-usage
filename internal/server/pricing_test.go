package server

import "testing"

// A 1h cache write bills at 2x input where the 5m default bills 1.25x.
// The 1h tokens are a subset of the cache-creation total, so only the
// premium may be added on top.
func TestCostAddsOnlyTheOneHourPremium(t *testing.T) {
	p, err := NewPricer("")
	if err != nil {
		t.Fatal(err)
	}
	const model = "claude-opus-4-1"

	all5m := p.Cost(model, 0, 0, 1_000_000, 0, 0)
	if all5m != 18.75 {
		t.Fatalf("5m-only cost = %v, want 18.75", all5m)
	}
	all1h := p.Cost(model, 0, 0, 1_000_000, 1_000_000, 0)
	if all1h != 30 {
		t.Errorf("1h cost = %v, want 30 (2x the 15/M input rate), not %v+30", all1h, all5m)
	}
	half := p.Cost(model, 0, 0, 1_000_000, 500_000, 0)
	if want := (18.75 + 30) / 2; half != want {
		t.Errorf("half-1h cost = %v, want %v", half, want)
	}
}

// Models without a 1h tier (every non-Anthropic one) must be unaffected
// even if a record somehow carries 1h tokens.
func TestCostIgnoresOneHourTokensWithoutAPremium(t *testing.T) {
	p, err := NewPricer("")
	if err != nil {
		t.Fatal(err)
	}
	with := p.Cost("gpt-4o", 1000, 1000, 0, 1000, 0)
	without := p.Cost("gpt-4o", 1000, 1000, 0, 0, 0)
	if with != without {
		t.Errorf("cost with 1h tokens = %v, without = %v; want equal", with, without)
	}
}
