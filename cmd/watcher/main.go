package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tokenusage/internal/watcher"
)

func main() {
	home, _ := os.UserHomeDir()
	defaultRoot := filepath.Join(home, ".claude", "projects")
	defaultStateDir := filepath.Join(home, ".token-usage-watcher")

	root := flag.String("root", defaultRoot, "Claude Code projects root")
	endpoint := flag.String("endpoint", env("TOKENUSAGE_ENDPOINT", "http://localhost:8080/ingest"), "server ingest endpoint")
	ckptPath := flag.String("checkpoint", filepath.Join(defaultStateDir, "checkpoint.json"), "checkpoint file path")
	bufDir := flag.String("buffer", filepath.Join(defaultStateDir, "buffer"), "offline buffer directory (empty disables buffering)")
	user := flag.String("user", env("TOKENUSAGE_USER", os.Getenv("USER")), "user id reported with each batch")
	machine := flag.String("machine", env("TOKENUSAGE_MACHINE", ""), "machine id (default: hostname)")
	interval := flag.Duration("interval", 5*time.Second, "scan interval")
	batchSize := flag.Int("batch", 200, "max records per upload batch")
	backfillCutoff := flag.Duration("backfill-cutoff", time.Hour, "records older than now-cutoff are marked backfill=true; 0 disables")
	authToken := flag.String("auth-token", os.Getenv("TOKENUSAGE_AUTH_TOKEN"), "optional bearer token (must match server)")
	once := flag.Bool("once", false, "scan once and exit (useful for cron / backfill)")
	flag.Parse()

	if *machine == "" {
		h, _ := os.Hostname()
		*machine = h
	}
	if *user == "" {
		log.Fatal("--user (or $TOKENUSAGE_USER/$USER) is required")
	}

	ckpt, err := watcher.LoadCheckpoint(*ckptPath)
	if err != nil {
		log.Fatalf("checkpoint: %v", err)
	}

	sc := &watcher.Scanner{
		Root:           *root,
		Checkpoint:     ckpt,
		BackfillCutoff: *backfillCutoff,
	}
	up := &watcher.Uploader{
		Endpoint:  *endpoint,
		MachineID: *machine,
		UserID:    *user,
		AuthToken: *authToken,
		Client:    &http.Client{Timeout: 30 * time.Second},
		BufferDir: *bufDir,
	}

	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	log.Printf("watcher: root=%s endpoint=%s user=%s machine=%s interval=%s",
		*root, *endpoint, *user, *machine, *interval)

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

func runOnce(ctx context.Context, sc *watcher.Scanner, up *watcher.Uploader, ckpt *watcher.Checkpoint, batchSize int) {
	up.Flush(ctx) // drain any prior offline buffer

	recs, pending, err := sc.Scan()
	if err != nil {
		log.Printf("scan: %v", err)
	}
	if len(recs) == 0 {
		// No new records, but offsets may have advanced past non-assistant lines —
		// persist them so we don't re-scan the same bytes next tick.
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

	allDurable := true
	for start := 0; start < len(recs); start += batchSize {
		end := min(start+batchSize, len(recs))
		if err := up.Send(ctx, recs[start:end]); err != nil {
			log.Printf("send: %v", err)
			allDurable = false
			break
		}
	}
	if !allDurable {
		// Don't advance offsets — server will dedup on next attempt anyway.
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

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
