package cli

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

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

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Print("shutdown signal received; finishing current scan… (Ctrl+C again to force-exit)")
		cancel()
		<-sigCh
		log.Print("second signal — exiting immediately")
		os.Exit(130)
	}()

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
	recs, pending, err := sc.Scan()
	if err != nil {
		log.Printf("scan: %v", err)
	}
	if len(recs) == 0 {
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

	batches := (len(recs) + batchSize - 1) / batchSize
	log.Printf("scan: %d records across %d files in %s — uploading in %d batch(es)",
		len(recs), len(pending), time.Since(scanStart).Round(time.Millisecond), batches)

	allDurable := true
	sendStart := time.Now()
	for start := 0; start < len(recs); start += batchSize {
		end := min(start+batchSize, len(recs))
		if err := up.Send(ctx, recs[start:end]); err != nil {
			log.Printf("send: %v", err)
			allDurable = false
			break
		}
		idx := start/batchSize + 1
		if idx == batches || idx%10 == 0 {
			log.Printf("  batch %d/%d sent (%d records, %s elapsed)",
				idx, batches, end, time.Since(sendStart).Round(time.Millisecond))
		}
	}
	if !allDurable {
		return
	}
	for p, s := range pending {
		ckpt.Set(p, s)
	}
	if err := ckpt.Save(); err != nil {
		log.Printf("save checkpoint: %v", err)
		return
	}
	log.Printf("ingested %d records across %d files", len(recs), len(pending))
}
