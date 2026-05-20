package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
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
	InputCostPerToken         *float64 `json:"input_cost_per_token"`
	OutputCostPerToken        *float64 `json:"output_cost_per_token"`
	CacheCreationCostPerToken *float64 `json:"cache_creation_input_token_cost"`
	CacheReadCostPerToken     *float64 `json:"cache_read_input_token_cost"`
	LiteLLMProvider           string   `json:"litellm_provider"`
	Mode                      string   `json:"mode"`
}

// PriceSyncMaxAttempts and PriceSyncInitialBackoff bound the retry loop
// inside one SyncPrices invocation. Even with full failure (10 retries),
// the worker still re-attempts next tick — these handle transient blips
// (DNS hiccup, CDN 5xx) within a single sync cycle.
const (
	PriceSyncMaxAttempts     = 10
	PriceSyncInitialBackoff  = 1 * time.Second
	PriceSyncMaxBackoff      = 30 * time.Second
)

// SyncPrices fetches the LiteLLM table (with retry), filters to
// Anthropic + OpenAI chat/completion entries, normalizes their names,
// and upserts each model's prices into the DB. Returns how many entries
// were considered vs. how many actually wrote a new history row.
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

	// Group normalized prefixes; if multiple LiteLLM entries collapse to
	// the same prefix, prefer the one that has cache pricing (more
	// recent, fully specified). Otherwise the first wins.
	type candidate struct {
		entry  liteLLMEntry
		source string // original LiteLLM key, kept for the `source` column
	}
	chosen := map[string]candidate{}

	for name, e := range raw {
		if e.Mode != "" && e.Mode != "chat" && e.Mode != "completion" {
			continue
		}
		if e.LiteLLMProvider != "anthropic" && e.LiteLLMProvider != "openai" {
			continue
		}
		if e.InputCostPerToken == nil || e.OutputCostPerToken == nil {
			continue
		}
		prefix := normalizeModel(name)
		if prefix == "" {
			continue
		}
		ex, exists := chosen[prefix]
		if !exists {
			chosen[prefix] = candidate{entry: e, source: name}
			continue
		}
		// Prefer entry that has cache pricing info if the previous didn't.
		if (ex.entry.CacheCreationCostPerToken == nil && ex.entry.CacheReadCostPerToken == nil) &&
			(e.CacheCreationCostPerToken != nil || e.CacheReadCostPerToken != nil) {
			chosen[prefix] = candidate{entry: e, source: name}
		}
	}

	considered = len(chosen)
	for prefix, c := range chosen {
		row := PriceRow{
			ModelPrefix:   prefix,
			ValidFrom:     time.Now(),
			InputPer1M:    *c.entry.InputCostPerToken * 1e6,
			OutputPer1M:   *c.entry.OutputCostPerToken * 1e6,
			CacheCreate1M: derefFloat(c.entry.CacheCreationCostPerToken) * 1e6,
			CacheRead1M:   derefFloat(c.entry.CacheReadCostPerToken) * 1e6,
			Source:        "litellm:" + c.source,
		}
		didChange, uerr := store.UpsertPrice(ctx, row)
		if uerr != nil {
			log.Printf("pricesync: upsert %s: %v", prefix, uerr)
			continue
		}
		if didChange {
			changed++
			log.Printf("pricesync: %s → in=%.4f out=%.4f cw=%.4f cr=%.4f (per 1M)",
				prefix, row.InputPer1M, row.OutputPer1M, row.CacheCreate1M, row.CacheRead1M)
		}
	}
	return considered, changed, nil
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
// matches the model strings Claude Code / Codex actually emit.
//   "anthropic/claude-3-5-sonnet-20241022"  -> "claude-3-5-sonnet"
//   "claude-opus-4-1-20250805"              -> "claude-opus-4-1"
//   "gpt-4o-2024-08-06"                     -> "gpt-4o"
//   "gpt-4o"                                -> "gpt-4o"
func normalizeModel(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[i+1:]
	}
	return dateSuffixRe.ReplaceAllString(s, "")
}

// `-20250805` (8 digits) OR `-2025-08-05` (YYYY-MM-DD)
var dateSuffixRe = regexp.MustCompile(`-(?:\d{8}|\d{4}-\d{2}-\d{2})$`)
