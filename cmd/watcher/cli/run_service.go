package cli

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/kardianos/service"

	"tokenusage/cmd/watcher/cli/svc"
)

// runUnderService dispatches between interactive (console) and service-managed
// (systemd / launchd / Windows SCM) execution. Interactive mode runs the loop
// directly with SIGINT/SIGTERM handling — same behavior the binary had before.
// Service mode hands lifecycle to kardianos so it can satisfy the Windows SCM
// start protocol; without that, SCM kills the process with error 1053 ("did
// not respond to the start or control request in a timely fashion") after 30s
// of no status callback. On Linux/macOS the loop reaches the same code path
// because systemd/launchd don't require the callback, but routing through
// kardianos also gets us journald/syslog log capture for free.
func runUnderService(loop func(context.Context) error) error {
	r := &serviceRunner{loop: loop}
	s, err := service.New(r, &service.Config{Name: svc.Name})
	if err != nil {
		return err
	}
	if service.Interactive() {
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
		return loop(ctx)
	}
	// Under SCM, stdout/stderr go to NUL — bridge the stdlib logger into
	// kardianos's service.Logger so messages reach the Windows Event Log
	// (provider = svc.Name, registered by kardianos at install time).
	if runtime.GOOS == "windows" {
		if sLogger, err := s.Logger(nil); err == nil {
			log.SetOutput(&serviceLogWriter{logger: sLogger})
			log.SetFlags(0) // event log adds its own timestamp
		}
	}
	return s.Run()
}

// serviceRunner adapts our loop to kardianos's service.Interface. Start spawns
// the loop in a goroutine and returns immediately (kardianos requires Start to
// be non-blocking so it can ack the service manager). Stop cancels the loop
// and waits for it to drain, bounded by a deadline so a stuck loop can't hang
// service shutdown.
type serviceRunner struct {
	loop   func(context.Context) error
	cancel context.CancelFunc
	done   chan struct{}
}

func (r *serviceRunner) Start(_ service.Service) error {
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		if err := r.loop(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("watcher loop exited: %v", err)
		}
	}()
	return nil
}

func (r *serviceRunner) Stop(_ service.Service) error {
	if r.cancel != nil {
		r.cancel()
	}
	select {
	case <-r.done:
	case <-time.After(15 * time.Second):
		log.Print("watcher loop didn't drain in 15s; abandoning")
	}
	return nil
}

// serviceLogWriter adapts service.Logger (Info/Warning/Error) to an io.Writer
// so the stdlib log package can drive it. Every log.Printf line lands as one
// event-log entry at Info level.
type serviceLogWriter struct{ logger service.Logger }

func (w *serviceLogWriter) Write(p []byte) (int, error) {
	_ = w.logger.Info(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}
