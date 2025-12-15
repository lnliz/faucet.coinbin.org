package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/lnliz/faucet.coinbin.org/db"
	"github.com/lnliz/faucet.coinbin.org/service"
)

type stringSlice []string

func (s *stringSlice) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSlice) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func main() {
	var cfg service.Config
	var adminIPAllowlist stringSlice
	var batchIntervalStr string
	var autoConsolidationIntervalStr string

	flag.StringVar(&cfg.ListenAddr, "listen", ":8080", "HTTP server listen address")
	flag.StringVar(&cfg.MetricsAddr, "metrics-addr", "0.0.0.0:9222", "Metrics server listen address")
	flag.StringVar(&cfg.DataDir, "data-dir", "./data", "Directory for data files (database, etc)")

	flag.StringVar(&cfg.BitcoinRPC.Host, "bitcoin-rpc-host", "localhost:38332", "Bitcoin Signet RPC host")
	flag.StringVar(&cfg.BitcoinRPC.User, "bitcoin-rpc-user", "", "Bitcoin RPC username")
	flag.StringVar(&cfg.BitcoinRPC.Password, "bitcoin-rpc-password", "", "Bitcoin RPC password")

	flag.StringVar(&batchIntervalStr, "batch-interval", "1m", "Batch processing interval (e.g., 1m, 5m, 30s)")
	flag.Float64Var(&cfg.MinAmountBTC, "min-amount", 0.0001, "Minimum send amount (BTC)")
	flag.Float64Var(&cfg.MaxAmountBTC, "max-amount", 0.0009, "Maximum send amount (BTC)")
	flag.Float64Var(&cfg.MinBalance, "min-balance", 0.1, "Minimum wallet balance threshold (BTC)")
	flag.Float64Var(&cfg.ConsolidationAmountThresholdBTC, "consolidation-amount-threshold", 0.001, "UTXO consolidation threshold (BTC) - UTXOs smaller than this will be consolidated")
	flag.IntVar(&cfg.MaxConsolidationUTXOs, "consolidation-max-utxos", 5, "Maximum number of UTXOs to consolidate in a single transaction")
	flag.IntVar(&cfg.MinConsolidationUTXOs, "consolidation-min-utxos", 2, "Minimum number of UTXOs required before consolidation runs")
	flag.StringVar(&autoConsolidationIntervalStr, "auto-consolidation-interval", "", "Auto-consolidation interval (e.g., 5m, 1h) - disabled by default")

	flag.StringVar(&cfg.TurnstileSecret, "turnstile-secret", "", "Cloudflare Turnstile secret key (optional)")
	flag.StringVar(&cfg.TurnstileSiteKey, "turnstile-site-key", "", "Cloudflare Turnstile site key (optional)")

	flag.StringVar(&cfg.AdminPassword, "admin-password", "", "Admin dashboard password (required)")
	flag.StringVar(&cfg.AdminPath, "admin-path", "/admin", "Admin dashboard URL path")
	flag.StringVar(&cfg.AdminCookieSecret, "admin-cookie-secret", "", "Admin cookie signing secret (required, 32+ chars)")
	flag.StringVar(&cfg.Admin2FASecret, "admin-2fa-secret", "", "Admin 2FA TOTP secret (optional, base32 encoded)")
	flag.Var(&adminIPAllowlist, "admin-ip", "Allowed IP for admin access (can be specified multiple times, default: 127.0.0.1)")

	flag.Parse()

	if cfg.MinConsolidationUTXOs > cfg.MaxConsolidationUTXOs {
		log.Fatal("invalid consolidation cfg, min: %d > max: %d", cfg.MinConsolidationUTXOs, cfg.MaxConsolidationUTXOs)
	}

	if cfg.MinAmountBTC > cfg.MaxAmountBTC {
		log.Fatal("invalid cfg, min: %.8f > max: %.8f", cfg.MinAmountBTC, cfg.MaxAmountBTC)
	}

	if len(adminIPAllowlist) == 0 {
		adminIPAllowlist = []string{"127.0.0.1"}
	}
	cfg.AdminIPAllowlist = adminIPAllowlist

	if cfg.AdminPassword == "" {
		log.Fatal("Error: -admin-password flag is required")
	}
	if cfg.AdminCookieSecret == "" {
		log.Fatal("Error: -admin-cookie-secret flag is required")
	}
	if len(cfg.AdminCookieSecret) < 32 {
		log.Fatal("Error: -admin-cookie-secret must be at least 32 characters")
	}
	if cfg.BitcoinRPC.User == "" {
		log.Fatal("Error: -bitcoin-rpc-user flag is required")
	}
	if cfg.BitcoinRPC.Password == "" {
		log.Fatal("Error: -bitcoin-rpc-password flag is required")
	}

	batchInterval, err := time.ParseDuration(batchIntervalStr)
	if err != nil {
		log.Fatalf("Error: invalid -batch-interval: %v", err)
	}
	cfg.BatchInterval = batchInterval

	if autoConsolidationIntervalStr != "" {
		autoConsolidationInterval, err := time.ParseDuration(autoConsolidationIntervalStr)
		if err != nil {
			log.Fatalf("Error: invalid -auto-consolidation-interval: %v", err)
		}
		cfg.AutoConsolidationInterval = autoConsolidationInterval
	}

	log.Printf("Signet Bitcoin Faucet starting...")
	log.Printf("CommitHash: %s", service.CommitHash)
	log.Printf("Listen address: %s", cfg.ListenAddr)
	log.Printf("Metrics address: %s", cfg.MetricsAddr)
	log.Printf("Data directory: %s", cfg.DataDir)
	log.Printf("Batch interval: %s", cfg.BatchInterval)
	log.Printf("Send amount range: %.8f - %.8f BTC", cfg.MinAmountBTC, cfg.MaxAmountBTC)
	log.Printf("Admin path: %s", cfg.AdminPath)
	if cfg.Admin2FASecret != "" {
		log.Printf("2FA enabled for admin login and send funds")
	}

	database, err := db.InitDB(cfg.DataDir)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	log.Println("Database initialized successfully")

	svc := service.NewService(&cfg, database)

	if err := svc.CheckBitcoinConnection(); err != nil {
		log.Fatalf("Bitcoin RPC connection failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup

	svc.StartBatchProcessor(ctx, &wg)
	svc.StartBalanceRefresher(ctx, &wg)
	if cfg.AutoConsolidationInterval > 0 {
		svc.StartAutoConsolidation(ctx, &wg)
	}
	svc.StartMetricsHttpServer()

	httpServer := svc.StartService()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	<-sigChan
	log.Println("Received shutdown signal, initiating graceful shutdown...")

	cancel()

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server shutdown error: %v", err)
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		log.Println("All background tasks completed")
	case <-shutdownCtx.Done():
		log.Println("Shutdown timeout exceeded, forcing exit")
	}

	log.Println("Shutdown complete")
}
