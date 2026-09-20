package server

import "testing"

func ptr(v float64) *float64 { return &v }

// The vendor's own key, two cloud aliases of it, a reseller markup and a
// partial entry — all collapsing onto "claude-haiku-4-5".
func haikuAliases() map[string]liteLLMEntry {
	return map[string]liteLLMEntry{
		"claude-haiku-4-5": {
			InputCostPerToken: ptr(1e-06), OutputCostPerToken: ptr(5e-06),
			CacheCreationCostPerToken: ptr(1.25e-06), CacheCreation1hCostPerToken: ptr(2e-06),
			CacheReadCostPerToken: ptr(1e-07), Mode: "chat",
		},
		"vertex_ai/claude-haiku-4-5": {
			InputCostPerToken: ptr(1e-06), OutputCostPerToken: ptr(5e-06),
			CacheCreationCostPerToken: ptr(1.25e-06), CacheCreation1hCostPerToken: ptr(2e-06),
			CacheReadCostPerToken: ptr(1e-07), Mode: "chat",
		},
		"snowflake/claude-haiku-4-5": {
			InputCostPerToken: ptr(1e-06), OutputCostPerToken: ptr(5e-06),
			CacheReadCostPerToken: ptr(1e-07), Mode: "chat",
		},
		"aihubmix/claude-haiku-4-5": {
			InputCostPerToken: ptr(1.1e-06), OutputCostPerToken: ptr(5.5e-06),
			CacheCreationCostPerToken: ptr(1.375e-06), CacheReadCostPerToken: ptr(1.1e-07),
			Mode: "chat",
		},
	}
}

// Map order used to decide the winner, so the same input produced
// different prices run to run and churned model_prices history.
func TestSelectPriceRowsIsDeterministicAcrossRuns(t *testing.T) {
	raw := haikuAliases()
	first := selectPriceRows(raw)
	for i := 0; i < 50; i++ {
		got := selectPriceRows(raw)
		if len(got) != len(first) {
			t.Fatalf("run %d returned %d rows, want %d", i, len(got), len(first))
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d row %d = %+v, want %+v", i, j, got[j], first[j])
			}
		}
	}
}

func TestSelectPriceRowsPrefersTheVendorEntry(t *testing.T) {
	rows := selectPriceRows(haikuAliases())
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 (every alias collapses): %+v", len(rows), rows)
	}
	got := rows[0]
	if got.ModelPrefix != "claude-haiku-4-5" {
		t.Errorf("prefix = %q", got.ModelPrefix)
	}
	if got.Source != "litellm:claude-haiku-4-5" {
		t.Errorf("source = %q, want the unprefixed vendor key", got.Source)
	}
	if got.InputPer1M != 1 || got.CacheCreate1M != 1.25 || got.CacheCreate1h1M != 2 {
		t.Errorf("rates = %+v, want in=1 cw=1.25 cw1h=2 (not the reseller markup)", got)
	}
}

// A partial entry must not beat a complete one even when it sorts first.
func TestSelectPriceRowsPrefersCompleteCacheTariff(t *testing.T) {
	raw := map[string]liteLLMEntry{
		"aaa/some-model": {
			InputCostPerToken: ptr(1e-06), OutputCostPerToken: ptr(2e-06), Mode: "chat",
		},
		"zzz/some-model": {
			InputCostPerToken: ptr(1e-06), OutputCostPerToken: ptr(2e-06),
			CacheCreationCostPerToken: ptr(1.25e-06), CacheReadCostPerToken: ptr(1e-07),
			Mode: "chat",
		},
	}
	rows := selectPriceRows(raw)
	if len(rows) != 1 || rows[0].Source != "litellm:zzz/some-model" {
		t.Fatalf("rows = %+v, want the entry carrying cache pricing", rows)
	}
}
