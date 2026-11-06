package pricing

import (
	"math"
	"testing"
)

func TestLongestPrefixWins(t *testing.T) {
	mini, ok := RateFor("gpt-4o-mini-2024-07-18")
	if !ok || mini.Input != 0.15 {
		t.Errorf("gpt-4o-mini rate = %+v, %v", mini, ok)
	}
	full, ok := RateFor("gpt-4o-2024-08-06")
	if !ok || full.Input != 2.50 {
		t.Errorf("gpt-4o rate = %+v, %v", full, ok)
	}
}

func TestVendorPrefixAndCase(t *testing.T) {
	rate, ok := RateFor("DeepSeek/DeepSeek-Chat")
	if !ok || rate.Output != 1.10 {
		t.Errorf("rate = %+v, ok = %v", rate, ok)
	}
}

func TestUnknownModelIsNotCharged(t *testing.T) {
	if _, ok := RateFor("my-private-finetune"); ok {
		t.Error("unknown model reported a known rate")
	}
	if cost, known := Estimate("my-private-finetune", 1000, 1000); known || cost != 0 {
		t.Errorf("cost = %v, known = %v", cost, known)
	}
}

func TestEstimate(t *testing.T) {
	// gpt-4o-mini: 0.15 input / 0.60 output per 1M tokens.
	cost, known := Estimate("gpt-4o-mini", 1_000_000, 1_000_000)
	if !known {
		t.Fatal("expected a known model")
	}
	if math.Abs(cost-0.75) > 1e-9 {
		t.Errorf("cost = %v, want 0.75", cost)
	}
}
