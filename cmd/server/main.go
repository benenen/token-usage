package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"tokenusage/internal/server"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

// Shared DSN: a persistent flag on root, inherited by every subcommand
// (server itself + admin/*). Defaults to $TOKENUSAGE_DSN.
var dsn string

func newRootCmd() *cobra.Command {
	var (
		addr            string
		pricePath       string
		rebuildToday    time.Duration
		rebuildDeepHour int
		rebuildDeepDays int
	)
	root := &cobra.Command{
		Use:   "token-usage-server",
		Short: "Token-usage central server: HTTP API + dashboard + admin CLI",
		Long: "Default: runs the HTTP server.\n" +
			"Subcommand `admin` manages users and API keys (see `admin --help`).",
		SilenceUsage: true,
		// PersistentPreRunE runs before every subcommand's RunE (including
		// root's own). One central spot to require --dsn for the whole CLI.
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			if dsn == "" {
				return errors.New("--dsn (or $TOKENUSAGE_DSN) is required")
			}
			return nil
		},
		RunE: func(c *cobra.Command, _ []string) error {
			return runServer(addr, pricePath, rebuildToday, rebuildDeepHour, rebuildDeepDays)
		},
	}
	root.PersistentFlags().StringVar(&dsn, "dsn", os.Getenv("TOKENUSAGE_DSN"),
		"PostgreSQL DSN (env: TOKENUSAGE_DSN) — required")
	root.Flags().StringVar(&addr, "addr", ":8080", "listen address")
	root.Flags().StringVar(&pricePath, "pricing", os.Getenv("TOKENUSAGE_PRICING"),
		"optional pricing override JSON")
	root.Flags().DurationVar(&rebuildToday, "rebuild-today-every", 10*time.Minute,
		"how often to recompute today's usage_daily; 0 disables")
	root.Flags().IntVar(&rebuildDeepHour, "rebuild-deep-hour", 8,
		"local-clock hour (0-23) for the daily deep rebuild; -1 disables")
	root.Flags().IntVar(&rebuildDeepDays, "rebuild-deep-days", 3,
		"how many days back (incl. today) the deep rebuild covers")
	root.Flags().DurationVar(&priceSyncEvery, "price-sync-every", 12*time.Hour,
		"how often to refresh model_prices from --price-source; 0 disables")
	root.Flags().StringVar(&priceSource, "price-source", server.DefaultPriceSourceURL,
		"URL of the LiteLLM-format model_prices_and_context_window.json")

	root.AddCommand(newAdminCmd())
	return root
}

// price-sync flags lifted to package scope so admin price-sync (manual
// trigger) and the server runner share defaults.
var (
	priceSyncEvery time.Duration
	priceSource    string
)

func runServer(addr, pricePath string, rebuildToday time.Duration, deepHour, deepDays int) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := server.NewStore(ctx, dsn)
	if err != nil {
		return fmt.Errorf("store: %w", err)
	}
	defer store.Close()

	pricer, err := server.NewPricer(pricePath)
	if err != nil {
		return fmt.Errorf("pricer: %w", err)
	}
	// Seed model_prices on first start so /summary's LATERAL JOIN always
	// finds a row even before the first LiteLLM fetch runs. After the
	// first sync the seeded rows get superseded by real ones.
	if err := store.SeedDefaultPrices(ctx, server.DefaultRates()); err != nil {
		log.Printf("price seed warning: %v", err)
	}

	api := &server.API{Store: store, Pricer: pricer}
	mux := http.NewServeMux()
	api.Register(mux)

	worker := &server.Worker{
		Store:       store,
		TodayEvery:  rebuildToday,
		DeepHour:    deepHour,
		DeepDays:    deepDays,
		PriceEvery:  priceSyncEvery,
		PriceSource: priceSource,
	}
	worker.Start(ctx)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Printf("token-usage server listening on %s", addr)
		if e := srv.ListenAndServe(); e != nil && e != http.ErrServerClosed {
			errCh <- e
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	select {
	case <-sigCh:
		log.Print("shutdown signal received")
	case e := <-errCh:
		return e
	}
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	return srv.Shutdown(shutCtx)
}
