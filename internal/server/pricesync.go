package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// DefaultPriceSourceURL is the LiteLLM-maintained per-token price table.
// It's the de-facto source for Claude Code and ccusage too, updated
// reasonably promptly when Anthropic / OpenAI change list prices.
const DefaultPriceSourceURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"

// liteLLMEntry mirrors the relevant fields of one model in the LiteLLM
// JSON. Pointers so we can tell "missing" from "zero" — many entries
// omit cache pricing entirely.
type liteLLMEntry struct {
	InputCostPerToken           *float64 `json:"input_cost_per_token"`
	OutputCostPerToken          *float64 `json:"output_cost_per_token"`
	CacheCreationCostPerToken   *float64 `json:"cache_creation_input_token_cost"`
	CacheCreation1hCostPerToken *float64 `json:"cache_creation_input_token_cost_above_1hr"`
	CacheReadCostPerToken       *float64 `json:"cache_read_input_token_cost"`
	LiteLLMProvider             string   `json:"litellm_provider"`
	Mode                        string   `json:"mode"`
}

// PriceSyncMaxAttempts and PriceSyncInitialBackoff bound the retry loop
// inside one SyncPrices invocation. Even with full failure (10 retries),
// the worker still re-attempts next tick — these handle transient blips
// (DNS hiccup, CDN 5xx) within a single sync cycle.
const (
	PriceSyncMaxAttempts    = 10
	PriceSyncInitialBackoff = 1 * time.Second
	PriceSyncMaxBackoff     = 30 * time.Second
)

// SyncPrices fetches the LiteLLM table (with retry), filters to any
// chat/completion entry (across every provider — Anthropic, OpenAI,
// Google, DeepSeek, Together, Cohere, Mistral, Groq, Bedrock, etc.),
// normalizes their names, and upserts each model's prices into the
// DB. Models that aren't in this synced set and don't match any
// `defaultRates` prefix are priced at $0 via the `/summary`
// LATERAL-JOIN's COALESCE — by design, an unknown model never blows
// up totals with a stale guess. Returns how many entries were
// considered vs. how many actually wrote a new history row.
func SyncPrices(ctx context.Context, store *Store, sourceURL string, client *http.Client) (considered, changed int, err error) {
	if sourceURL == "" {
		sourceURL = DefaultPriceSourceURL
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}

	raw, err := fetchPriceJSONWithRetry(ctx, sourceURL, client)
	if err != nil {
		return 0, 0, err
	}

	rows := selectPriceRows(raw)

	considered = len(rows)
	for _, row := range rows {
		prefix := row.ModelPrefix
		row.ValidFrom = time.Now()
		didChange, uerr := store.UpsertPrice(ctx, row)
		if uerr != nil {
			log.Printf("pricesync: upsert %s: %v", prefix, uerr)
			continue
		}
		if didChange {
			changed++
			log.Printf("pricesync: %s → in=%.4f out=%.4f cw=%.4f cw1h=%.4f cr=%.4f (per 1M)",
				prefix, row.InputPer1M, row.OutputPer1M, row.CacheCreate1M,
				row.CacheCreate1h1M, row.CacheRead1M)
		}
	}
	return considered, changed, nil
}

// selectPriceRows turns the LiteLLM table into one PriceRow per
// normalized model prefix.
//
// Several LiteLLM keys routinely collapse onto the same prefix — the
// vendor's own entry plus vertex_ai/, azure_ai/, snowflake/, aihubmix/…
// aliases of it — and they disagree about both rates and which cache
// fields they carry. Picking by Go's map order (what this used to do)
// made the winner, and therefore the price, flip on every sync:
// gpt-5.6-sol alternated between $4 and $5 input for months, writing a
// fresh model_prices history row each time and re-pricing whole days of
// usage with it. The choice is now deterministic: rank by pricing
// completeness, prefer the vendor's unprefixed key, and settle ties by
// sorted key order.
func selectPriceRows(raw map[string]liteLLMEntry) []PriceRow {
	type candidate struct {
		entry  liteLLMEntry
		source string // original LiteLLM key, kept for the `source` column
		rank   int
	}
	chosen := map[string]candidate{}

	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		e := raw[name]
		if e.Mode != "" && e.Mode != "chat" && e.Mode != "completion" {
			continue
		}
		if e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
			continue
		}
		prefix := normalizeModel(name)
		if prefix == "" {
			continue
		}
		c := candidate{entry: e, source: name, rank: priceCandidateRank(name, e)}
		if ex, exists := chosen[prefix]; exists && ex.rank >= c.rank {
			continue
		}
		chosen[prefix] = c
	}

	out := make([]PriceRow, 0, len(chosen))
	for prefix, c := range chosen {
		out = append(out, PriceRow{
			ModelPrefix:     prefix,
			InputPer1M:      *c.entry.InputCostPerToken * 1e6,
			OutputPer1M:     *c.entry.OutputCostPerToken * 1e6,
			CacheCreate1M:   derefFloat(c.entry.CacheCreationCostPerToken) * 1e6,
			CacheCreate1h1M: derefFloat(c.entry.CacheCreation1hCostPerToken) * 1e6,
			CacheRead1M:     derefFloat(c.entry.CacheReadCostPerToken) * 1e6,
			Source:          "litellm:" + c.source,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModelPrefix < out[j].ModelPrefix })
	return out
}

// priceCandidateRank scores one LiteLLM entry competing for a prefix:
// a fully specified cache tariff beats a partial one, and the vendor's
// own key beats a reseller alias of it. Equal scores are settled by the
// caller's sorted iteration, never by map order.
func priceCandidateRank(key string, e liteLLMEntry) int {
	rank := 0
	if e.CacheCreationCostPerToken != nil {
		rank += 8
	}
	if e.CacheCreation1hCostPerToken != nil {
		rank += 4
	}
	if e.CacheReadCostPerToken != nil {
		rank += 2
	}
	if !strings.Contains(key, "/") {
		rank++
	}
	return rank
}

// fetchPriceJSONWithRetry retries up to PriceSyncMaxAttempts times with
// exponential backoff, capped at PriceSyncMaxBackoff. ctx.Done aborts
// immediately. 4xx are NOT retried (the URL is wrong); 5xx and transport
// errors are.
func fetchPriceJSONWithRetry(ctx context.Context, sourceURL string, client *http.Client) (map[string]liteLLMEntry, error) {
	backoff := PriceSyncInitialBackoff
	var lastErr error
	for attempt := 1; attempt <= PriceSyncMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw, retry, err := fetchPriceJSONOnce(ctx, sourceURL, client)
		if err == nil {
			if attempt > 1 {
				log.Printf("pricesync: fetched after %d attempt(s)", attempt)
			}
			return raw, nil
		}
		lastErr = err
		if !retry || attempt == PriceSyncMaxAttempts {
			break
		}
		log.Printf("pricesync: attempt %d/%d failed: %v; retrying in %s",
			attempt, PriceSyncMaxAttempts, err, backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > PriceSyncMaxBackoff {
			backoff = PriceSyncMaxBackoff
		}
	}
	return nil, fmt.Errorf("after %d attempts: %w", PriceSyncMaxAttempts, lastErr)
}

// fetchPriceJSONOnce returns (raw, retryable, err).
// retryable=false for 4xx (won't change on retry).
func fetchPriceJSONOnce(ctx context.Context, sourceURL string, client *http.Client) (map[string]liteLLMEntry, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, true, fmt.Errorf("fetch %s: %w", sourceURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		retry := resp.StatusCode >= 500 || resp.StatusCode == 408 || resp.StatusCode == 429
		return nil, retry, fmt.Errorf("fetch %s: HTTP %d: %s", sourceURL, resp.StatusCode, body)
	}
	var raw map[string]liteLLMEntry
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, false, fmt.Errorf("decode JSON: %w", err)
	}
	return raw, false, nil
}

func derefFloat(p *float64) float64 {
	if p == nil {
		return 0
	}
	return *p
}

// normalizeModel strips provider prefixes and trailing version-date
// suffixes so LiteLLM keys collapse into a stable family identifier that
// matches the model strings Claude Code / Codex / pi actually emit.
//
//	"anthropic/claude-3-5-sonnet-20241022"     -> "claude-3-5-sonnet"
//	"openrouter/deepseek/deepseek-v4.1-flash"  -> "deepseek-v4.1-flash"
//	"claude-opus-4-1-20250805"                 -> "claude-opus-4-1"
//	"gpt-4o-2024-08-06"                        -> "gpt-4o"
//	"gpt-4o"                                   -> "gpt-4o"
//
// Every path segment goes, not just the first: a two-segment key like
// openrouter/deepseek/… otherwise keeps a slash that no emitted model
// name can ever match, which is why pi's deepseek-v4.1-flash — 331M
// tokens of it — priced at $0.
func normalizeModel(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return dateSuffixRe.ReplaceAllString(s, "")
}

// `-20250805` (8 digits) OR `-2025-08-05` (YYYY-MM-DD)
var dateSuffixRe = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)
