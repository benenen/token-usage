package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"tokenusage/internal/types"
	"tokenusage/internal/watcher"
)

// RunConfig holds every flag value the `run` (and root) command(s) parse.
// Exported so main.go can declare one instance and share it across both
// command bindings — typing `token-usage-watcher` and
// `token-usage-watcher run` then dispatch into the same struct.
type RunConfig struct {
	Sources        []string
	Root, Tool     string
	Endpoint       string
	APIKey         string
	Checkpoint     string
	BufferDir      string
	Interval       time.Duration
	Batch          int
	BackfillCutoff time.Duration
	Once           bool
}

// BindRunFlags attaches every `run`-mode flag to cmd, with values landing
// in cfg. Called once for the root cmd and once for the `run` subcommand
// so the two stay in lockstep without duplicating definitions.
func BindRunFlags(cmd *cobra.Command, cfg *RunConfig, defaultRoot, stateDir string) {
	cmd.Flags().StringSliceVar(&cfg.Sources, "source", nil,
		`repeatable: tool source as "tool=path". If absent (and no --root/--tool), auto-detect every known tool dir under $HOME.`)
	cmd.Flags().StringVar(&cfg.Root, "root", defaultRoot,
		"(single-source mode) JSONL root directory")
	cmd.Flags().StringVar(&cfg.Tool, "tool", envOr("TOKENUSAGE_TOOL", "claude-code"),
		"(single-source mode) tool tag stamped on records from --root")
	cmd.Flags().StringVar(&cfg.Endpoint, "endpoint",
		envOr("TOKENUSAGE_ENDPOINT", "http://localhost:8080/ingest"),
		"server ingest endpoint")
	cmd.Flags().StringVar(&cfg.APIKey, "api-key", os.Getenv("TOKENUSAGE_API_KEY"),
		"API key (tuk_…) — required. Mint via: token-usage-server admin key-create <user>")
	cmd.Flags().StringVar(&cfg.Checkpoint, "checkpoint",
		filepath.Join(stateDir, "checkpoint.json"), "checkpoint file path")
	cmd.Flags().StringVar(&cfg.BufferDir, "buffer",
		filepath.Join(stateDir, "buffer"),
		"offline buffer directory (empty disables buffering)")
	cmd.Flags().DurationVar(&cfg.Interval, "interval", 5*time.Second, "scan interval")
	cmd.Flags().IntVar(&cfg.Batch, "batch", 200, "max records per upload batch")
	cmd.Flags().DurationVar(&cfg.BackfillCutoff, "backfill-cutoff", time.Hour,
		"records older than now-cutoff are tagged backfill=true; 0 disables")
	cmd.Flags().BoolVar(&cfg.Once, "once", false, "scan once and exit (cron / manual backfill)")
}

// DoRun is the watcher's resident loop. Wired as RunE for both the root
// command (no subcommand) and the explicit `run` subcommand.
func DoRun(c *cobra.Command, cfg *RunConfig, home string) error {
	sources, err := resolveSources(c, cfg, home)
	if err != nil {
		return err
	}
	if cfg.APIKey == "" {
		return errors.New("--api-key (or $TOKENUSAGE_API_KEY) is required. Mint with: token-usage-server admin key-create <user>")
	}
	machine, err := os.Hostname()
	if err != nil || machine == "" {
		return fmt.Errorf("hostname: %w", err)
	}

	ckpt, err := watcher.LoadCheckpoint(cfg.Checkpoint)
	if err != nil {
		return fmt.Errorf("checkpoint: %w", err)
	}
	sc := &watcher.Scanner{
		Sources:        sources,
		Checkpoint:     ckpt,
		BackfillCutoff: cfg.BackfillCutoff,
	}
	up := &watcher.Uploader{
		Endpoint:  cfg.Endpoint,
		MachineID: machine,
		APIKey:    cfg.APIKey,
		Client:    &http.Client{Timeout: 30 * time.Second},
		BufferDir: cfg.BufferDir,
	}

	loop := func(ctx context.Context) error {
		log.Printf("watcher: endpoint=%s machine=%s key=%s sources=[%s] interval=%s",
			cfg.Endpoint, machine, displayKey(cfg.APIKey), formatSources(sources), cfg.Interval)
		tick := time.NewTicker(cfg.Interval)
		defer tick.Stop()
		for {
			runOnce(ctx, sc, up, ckpt, cfg.Batch)
			if cfg.Once {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			case <-tick.C:
			}
		}
	}
	return runUnderService(loop)
}

// resolveSources walks the precedence: explicit --source > --root/--tool > auto-detect.
func resolveSources(c *cobra.Command, cfg *RunConfig, home string) ([]watcher.Source, error) {
	if len(cfg.Sources) > 0 {
		return parseSources(cfg.Sources)
	}
	if c.Flags().Changed("root") || c.Flags().Changed("tool") {
		return []watcher.Source{{Tool: cfg.Tool, Root: cfg.Root}}, nil
	}
	auto := watcher.AutoDetectSources(home)
	if len(auto) > 0 {
		return auto, nil
	}
	// No known tool dirs found — keep historical default so something
	// runs and logs "scanned 0 files" rather than dying silently.
	return []watcher.Source{{Tool: cfg.Tool, Root: cfg.Root}}, nil
}

func parseSources(specs []string) ([]watcher.Source, error) {
	out := make([]watcher.Source, 0, len(specs))
	for _, s := range specs {
		i := strings.IndexByte(s, '=')
		if i <= 0 || i == len(s)-1 {
			return nil, fmt.Errorf(`--source must be "tool=path", got %q`, s)
		}
		out = append(out, watcher.Source{Tool: s[:i], Root: s[i+1:]})
	}
	return out, nil
}

func formatSources(ss []watcher.Source) string {
	parts := make([]string, len(ss))
	for i, s := range ss {
		parts[i] = fmt.Sprintf("%s→%s", s.Tool, s.Root)
	}
	return strings.Join(parts, ", ")
}

func displayKey(k string) string {
	if len(k) >= 12 {
		return k[:12] + "…"
	}
	return "[invalid]"
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func runOnce(ctx context.Context, sc *watcher.Scanner, up *watcher.Uploader, ckpt *watcher.Checkpoint, batchSize int) {
	up.Flush(ctx)

	scanStart := time.Now()
	scanned, pending, err := sc.Scan()
	if err != nil {
		log.Printf("scan: %v", err)
	}
	recs, edits := scanned.Usage, scanned.Edits
	elapsed := time.Since(scanStart).Round(time.Millisecond)

	// One scan-summary line per tick, regardless of whether anything
	// new flowed. Without this you can't tell a healthy idle watcher
	// from a hung one. Per-tool breakdown shows both files walked AND
	// new records — i.e. an idle claude-code source still shows up as
	// `claude-code 0/801`, proving it's being scanned.
	log.Printf("scan: %s in %s", formatPerToolScan(sc.Sources, pending, recs, edits), elapsed)

	if len(recs) == 0 && len(edits) == 0 {
		if len(pending) > 0 {
			for p, s := range pending {
				ckpt.Set(p, s)
			}
			if err := ckpt.Save(); err != nil {
				log.Printf("save checkpoint: %v", err)
			}
		}
		return
	}

	// Per-record breakdown so you can chase down "why didn't tool X grow"
	// at the source. Cheap (one line per assistant message) and
	// grep-friendly: `grep "ingest:.*tool=claude-code"` etc.
	for _, r := range recs {
		log.Printf("  ingest: tool=%s model=%s in=%d out=%d cc=%d cr=%d msg=%s session=%s project=%s backfill=%v",
			r.Tool, r.Model,
			r.InputTokens, r.OutputTokens, r.CacheCreationTokens, r.CacheReadTokens,
			r.MessageID, r.SessionID, r.ProjectPath, r.Backfill)
	}
	for _, e := range edits {
		log.Printf("  ingest-edit: tool=%s lang=%s +%d/-%d event=%s session=%s project=%s backfill=%v",
			e.Tool, e.Lang, e.LinesAdded, e.LinesRemoved,
			e.EventID, e.SessionID, e.ProjectPath, e.Backfill)
	}

	// Usage batches first, then edit batches — separate requests so a
	// pre-edits server still takes the usage stream (see Uploader.SendEdits).
	// The checkpoint only advances when EVERY batch of BOTH kinds is
	// durable (200 or spooled); on failure the server dedups re-sends.
	allDurable := sendBatched(len(recs), batchSize, "", func(start, end int) (watcher.SendResult, error) {
		return up.Send(ctx, recs[start:end])
	})
	if allDurable {
		allDurable = sendBatched(len(edits), batchSize, "edit ", func(start, end int) (watcher.SendResult, error) {
			return up.SendEdits(ctx, edits[start:end])
		})
	}
	if !allDurable {
		return
	}
	for p, s := range pending {
		ckpt.Set(p, s)
	}
	if err := ckpt.Save(); err != nil {
		log.Printf("save checkpoint: %v", err)
	}
	// (No "ingested N" summary line — per-batch "accepted=… duplicates=…"
	//  already says exactly what landed on the server.)
}

// sendBatched slices [0,n) into batchSize windows and sends each via
// send(), logging every batch's server tally. Returns false as soon as a
// batch is neither accepted nor spooled (caller must then NOT advance
// the checkpoint).
func sendBatched(n, batchSize int, kind string, send func(start, end int) (watcher.SendResult, error)) bool {
	if n == 0 {
		return true
	}
	batches := (n + batchSize - 1) / batchSize
	sendStart := time.Now()
	for start := 0; start < n; start += batchSize {
		end := min(start+batchSize, n)
		res, err := send(start, end)
		if err != nil {
			log.Printf("send: %v", err)
			return false
		}
		idx := start/batchSize + 1
		// Always log every batch's server-side accept/dup tally — that's
		// the only place "I sent N but only K were new on the server"
		// surfaces. Without this, dedup on the server is invisible and
		// looks like "I ingested 42 records but the dashboard didn't move".
		status := fmt.Sprintf("accepted=%d duplicates=%d", res.Accepted, res.Duplicates)
		if res.Spooled {
			status = fmt.Sprintf("spooled (post failed: %v) — will retry on next tick", res.SpoolReason)
		}
		log.Printf("  %sbatch %d/%d sent (%d records, %s elapsed): %s",
			kind, idx, batches, end-start, time.Since(sendStart).Round(time.Millisecond), status)
	}
	return true
}

// formatPerToolScan renders the per-tool "new/walked" summary used by
// runOnce's per-tick log line. Files are attributed to tools by
// longest-Source.Root prefix match on each pending path; records
// already carry their Tool stamp from the parser.
//
//	scan: claude-code 0/801, codex 0/12, opencode 0/1
//	scan: claude-code 5/801, codex 0/12, opencode 0/1
//
// Always emits every configured source, even ones with zero files and
// zero records, so an idle source is visibly being scanned.
func formatPerToolScan(sources []watcher.Source, pending map[string]watcher.FileState, recs []types.UsageRecord, edits []types.EditRecord) string {
	files := make(map[string]int, len(sources))
	news := make(map[string]int, len(sources))
	newEdits := make(map[string]int, len(sources))
	for _, src := range sources {
		files[src.Tool] = 0
		news[src.Tool] = 0
	}
	for path := range pending {
		if tool := attributePath(sources, path); tool != "" {
			files[tool]++
		}
	}
	for _, r := range recs {
		news[r.Tool]++
	}
	for _, e := range edits {
		newEdits[e.Tool]++
	}
	tools := make([]string, 0, len(files))
	for t := range files {
		tools = append(tools, t)
	}
	sort.Strings(tools)
	parts := make([]string, 0, len(tools))
	for _, t := range tools {
		s := fmt.Sprintf("%s %d/%d", t, news[t], files[t])
		if n := newEdits[t]; n > 0 {
			s += fmt.Sprintf(" (+%d edits)", n)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, ", ")
}

// attributePath picks the Source whose Root is the longest prefix of
// path — handles the (unlikely but possible) case where two configured
// sources nest, e.g. `--source codex=$HOME/.codex/sessions` and
// `--source codex-foo=$HOME/.codex/sessions/foo`.
func attributePath(sources []watcher.Source, path string) string {
	best := ""
	bestLen := -1
	for _, src := range sources {
		if len(src.Root) > bestLen && strings.HasPrefix(path, src.Root) {
			best = src.Tool
			bestLen = len(src.Root)
		}
	}
	return best
}
