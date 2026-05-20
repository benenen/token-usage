package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tokenusage/internal/watcher"
)

// sourceFlag accepts repeated --source TOOL=PATH flags. Today only the
// claude-code JSONL format is parseable; new tools (codex, opencode, …)
// just need a parser branch in internal/watcher/scanner.go.
type sourceFlag []watcher.Source

func (s *sourceFlag) String() string {
	parts := make([]string, len(*s))
	for i, src := range *s {
		parts[i] = src.Tool + "=" + src.Root
	}
	return strings.Join(parts, ",")
}

func (s *sourceFlag) Set(v string) error {
	i := strings.IndexByte(v, '=')
	if i <= 0 || i == len(v)-1 {
		return errors.New(`--source must be "tool=path", e.g. "claude-code=/root/.claude/projects"`)
	}
	*s = append(*s, watcher.Source{Tool: v[:i], Root: v[i+1:]})
	return nil
}

func main() {
	home, _ := os.UserHomeDir()
	defaultRoot := filepath.Join(home, ".claude", "projects")
	defaultStateDir := filepath.Join(home, ".token-usage-watcher")

	var sources sourceFlag
	flag.Var(&sources, "source", `repeatable: tool source as "tool=path". If no --source/--root/--tool is given, auto-detects every known tool dir present under $HOME (currently: claude-code at ~/.claude/projects, codex at ~/.codex/sessions).`)

	// Single-source convenience flags. Passing either of these switches
	// off auto-detect and pins the watcher to one source.
	root := flag.String("root", defaultRoot, "(single-source mode) JSONL root directory")
	tool := flag.String("tool", env("TOKENUSAGE_TOOL", "claude-code"), "(single-source mode) tool tag stamped on records from --root")

	endpoint := flag.String("endpoint", env("TOKENUSAGE_ENDPOINT", "http://localhost:8080/ingest"), "server ingest endpoint")
	ckptPath := flag.String("checkpoint", filepath.Join(defaultStateDir, "checkpoint.json"), "checkpoint file path")
	bufDir := flag.String("buffer", filepath.Join(defaultStateDir, "buffer"), "offline buffer directory (empty disables buffering)")
	apiKey := flag.String("api-key", os.Getenv("TOKENUSAGE_API_KEY"), "API key (tuk_…) — required. Mint with: token-usage-server admin key-create <user>")
	interval := flag.Duration("interval", 5*time.Second, "scan interval")
	batchSize := flag.Int("batch", 200, "max records per upload batch")
	backfillCutoff := flag.Duration("backfill-cutoff", time.Hour, "records older than now-cutoff are marked backfill=true; 0 disables")
	once := flag.Bool("once", false, "scan once and exit (useful for cron / backfill)")
	flag.Parse()

	if len(sources) == 0 {
		// Decide between single-source legacy mode (user passed --root or
		// --tool explicitly) and auto-detect mode (user passed nothing).
		explicit := map[string]bool{}
		flag.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
		if explicit["root"] || explicit["tool"] {
			sources = []watcher.Source{{Tool: *tool, Root: *root}}
		} else {
			sources = watcher.AutoDetectSources(home)
			if len(sources) == 0 {
				// No known tool dirs exist — keep the historical default
				// so the watcher logs something instead of dying silently.
				sources = []watcher.Source{{Tool: *tool, Root: *root}}
			}
		}
	}

	if *apiKey == "" {
		log.Fatal("--api-key (or $TOKENUSAGE_API_KEY) is required. Mint one with: token-usage-server admin key-create <user>")
	}
	machine, err := os.Hostname()
	if err != nil || machine == "" {
		log.Fatalf("could not determine hostname for machine id: %v", err)
	}

	ckpt, ckptErr := watcher.LoadCheckpoint(*ckptPath)
	if ckptErr != nil {
		log.Fatalf("checkpoint: %v", ckptErr)
	}

	// First run (no checkpoint) reads each JSONL from offset 0, so the
	// initial scan ships the full history. Subsequent runs only read the
	// new tail. Records older than --backfill-cutoff are tagged
	// backfill=true so they don't spike the live charts.
	sc := &watcher.Scanner{
		Sources:        sources,
		Checkpoint:     ckpt,
		BackfillCutoff: *backfillCutoff,
	}
	up := &watcher.Uploader{
		Endpoint:  *endpoint,
		MachineID: machine,
		APIKey:    *apiKey,
		Client:    &http.Client{Timeout: 30 * time.Second},
		BufferDir: *bufDir,
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
		*endpoint, machine, displayKey(*apiKey), formatSources(sources), *interval)

	tick := time.NewTicker(*interval)
	defer tick.Stop()

	for {
		runOnce(ctx, sc, up, ckpt, *batchSize)
		if *once {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func formatSources(sources []watcher.Source) string {
	parts := make([]string, len(sources))
	for i, s := range sources {
		parts[i] = fmt.Sprintf("%s→%s", s.Tool, s.Root)
	}
	return strings.Join(parts, ", ")
}

func runOnce(ctx context.Context, sc *watcher.Scanner, up *watcher.Uploader, ckpt *watcher.Checkpoint, batchSize int) {
	up.Flush(ctx)

	scanStart := time.Now()
	recs, pending, err := sc.Scan()
	if err != nil {
		log.Printf("scan: %v", err)
	}
	if len(recs) == 0 {
		// Offsets may still advance past non-assistant lines — persist them
		// so we don't re-scan the same bytes next tick.
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
		// progress every 10 batches (or always for the last one)
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

// displayKey returns a safe-to-log prefix for an api key.
func displayKey(k string) string {
	if len(k) >= 12 {
		return k[:12] + "…"
	}
	return "[invalid]"
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
