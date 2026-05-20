package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"tokenusage/internal/server"
)

func main() {
	// Subcommand dispatch: `token-usage-server admin <cmd> …`
	if len(os.Args) >= 2 && os.Args[1] == "admin" {
		runAdmin(os.Args[2:])
		return
	}
	runServer()
}

func runServer() {
	addr := flag.String("addr", ":8080", "listen address")
	dsn := flag.String("dsn", os.Getenv("TOKENUSAGE_DSN"), "PostgreSQL DSN, e.g. postgres://user:pass@host:5432/tokenusage?sslmode=disable")
	pricePath := flag.String("pricing", os.Getenv("TOKENUSAGE_PRICING"), "optional pricing override JSON")
	flag.Parse()

	if *dsn == "" {
		log.Fatal("--dsn (or TOKENUSAGE_DSN) is required")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	store, err := server.NewStore(ctx, *dsn)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer store.Close()

	pricer, err := server.NewPricer(*pricePath)
	if err != nil {
		log.Fatalf("pricer: %v", err)
	}

	api := &server.API{Store: store, Pricer: pricer}
	mux := http.NewServeMux()
	api.Register(mux)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("token-usage server listening on %s", *addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	log.Print("shutdown signal received")
	shutCtx, shutCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)
}
