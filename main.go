package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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

func getEnvOrFlag(flagVal, envName string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envName)
}

func main() {
	var cfg service.Config
	var adminIPAllowlist stringSlice
	var enabledAmountRangesStr string
	var batchIntervalStr string
	var autoConsolidationIntervalStr string

	flag.StringVar(&cfg.ListenAddr, "listen", ":8080", "HTTP server listen address")
	flag.StringVar(&cfg.MetricsAddr, "metrics-addr", "0.0.0.0:9222", "Metrics server listen address")
	flag.StringVar(&cfg.DataDir, "data-dir", "./data", "Directory for data files (database, etc)")

	flag.StringVar(&cfg.BitcoinRPC.Host, "bitcoin-rpc-host", "localhost:38332", "Bitcoin Signet RPC host")
	flag.StringVar(&cfg.BitcoinRPC.User, "bitcoin-rpc-user", "", "Bitcoin RPC username")
	flag.StringVar(&cfg.BitcoinRPC.Password, "bitcoin-rpc-password", "", "Bitcoin RPC password")
	flag.StringVar(&cfg.BitcoinCoreWalletName, "bitcoin-wallet-name", "faucet", "Bitcoin wallet name, will be loaded at start")

	flag.StringVar(&batchIntervalStr, "batch-interval", "1m", "Batch processing interval (e.g., 1m, 5m, 30s)")
	flag.StringVar(&enabledAmountRangesStr, "enabled-amount-ranges", "1,2,3", "Comma-separated amount ranges to enable (1=0.001-0.009, 2=0.01-0.09, 3=0.1-0.9, 4=1.0-2.0)")
	flag.IntVar(&cfg.DefaultAmountRange, "default-amount-range", 2, "Default selected amount range (1-4)")
	flag.Float64Var(&cfg.MinBalance, "min-balance", 0.1, "Minimum wallet balance threshold (BTC)")
	flag.Float64Var(&cfg.ConsolidationAmountThresholdBTC, "consolidation-amount-threshold", 0.001, "UTXO consolidation threshold (BTC) - UTXOs smaller than this will be consolidated")
	flag.IntVar(&cfg.MaxConsolidationUTXOs, "consolidation-max-utxos", 5, "Maximum number of UTXOs to consolidate in a single transaction")
	flag.IntVar(&cfg.MinConsolidationUTXOs, "consolidation-min-utxos", 2, "Minimum number of UTXOs required before consolidation runs")
	flag.StringVar(&autoConsolidationIntervalStr, "auto-consolidation-interval", "", "Auto-consolidation interval (e.g., 5m, 1h) - disabled by default")

	flag.IntVar(&cfg.MaxWithdrawalsPerIP24h, "max-withdrawals-per-ip-24h", 2, "Maximum number of withdrawals per IP per 24h")
	flag.IntVar(&cfg.MaxDepositsPerAddress, "max-deposits-per-address", 5, "Maximum number of deposits per address")

	flag.StringVar(&cfg.TurnstileSecret, "turnstile-secret", "", "Cloudflare Turnstile secret key (optional)")
	flag.StringVar(&cfg.TurnstileSiteKey, "turnstile-site-key", "", "Cloudflare Turnstile site key (optional)")

	flag.StringVar(&cfg.AdminPassword, "admin-password", "", "Admin dashboard password (required)")
	flag.StringVar(&cfg.AdminPath, "admin-path", "", "Admin dashboard URL path (default: /admin)")
	flag.StringVar(&cfg.AdminCookieSecret, "admin-cookie-secret", "", "Admin cookie signing secret (required, 32+ chars)")
	flag.StringVar(&cfg.Admin2FASecret, "admin-2fa-secret", "", "Admin 2FA TOTP secret (optional, base32 encoded)")
	flag.Var(&adminIPAllowlist, "admin-ip", "Allowed IP for admin access (can be specified multiple times, default: 127.0.0.1)")

	flag.Parse()

	cfg.BitcoinRPC.User = getEnvOrFlag(cfg.BitcoinRPC.User, "FAUCET_BITCOIN_RPC_USER")
	cfg.BitcoinRPC.Password = getEnvOrFlag(cfg.BitcoinRPC.Password, "FAUCET_BITCOIN_RPC_PASSWORD")
	cfg.TurnstileSecret = getEnvOrFlag(cfg.TurnstileSecret, "FAUCET_TURNSTILE_SECRET")
	cfg.TurnstileSiteKey = getEnvOrFlag(cfg.TurnstileSiteKey, "FAUCET_TURNSTILE_SITE_KEY")
	cfg.AdminPassword = getEnvOrFlag(cfg.AdminPassword, "FAUCET_ADMIN_PASSWORD")
	cfg.AdminPath = getEnvOrFlag(cfg.AdminPath, "FAUCET_ADMIN_PATH")
	if cfg.AdminPath == "" {
		cfg.AdminPath = "/admin"
	}
	cfg.AdminCookieSecret = getEnvOrFlag(cfg.AdminCookieSecret, "FAUCET_ADMIN_COOKIE_SECRET")
	cfg.Admin2FASecret = getEnvOrFlag(cfg.Admin2FASecret, "FAUCET_ADMIN_2FA_SECRET")

	if cfg.MinConsolidationUTXOs > cfg.MaxConsolidationUTXOs {
		log.Fatalf("invalid consolidation cfg, min: %d > max: %d", cfg.MinConsolidationUTXOs, cfg.MaxConsolidationUTXOs)
	}

	if len(adminIPAllowlist) == 0 {
		adminIPAllowlist = []string{"127.0.0.1"}
	}
	cfg.AdminIPAllowlist = adminIPAllowlist

	for _, r := range strings.Split(enabledAmountRangesStr, ",") {
		r = strings.TrimSpace(r)
		if r == "" {
			continue
		}
		rangeID, err := strconv.Atoi(r)
		if err != nil || rangeID < 1 || rangeID > 4 {
			log.Fatalf("Error: invalid -enabled-amount-ranges value: %s (must be 1-4)", r)
		}
		cfg.EnabledAmountRanges = append(cfg.EnabledAmountRanges, rangeID)
	}

	validDefault := false
	for _, r := range cfg.EnabledAmountRanges {
		if r == cfg.DefaultAmountRange {
			validDefault = true
			break
		}
	}
	if !validDefault {
		log.Fatalf("Error: -default-amount-range %d is not in enabled amount ranges", cfg.DefaultAmountRange)
	}

	if cfg.AdminPassword == "" {
		log.Fatal("Error: admin password required (use -admin-password or FAUCET_ADMIN_PASSWORD)")
	}
	if cfg.AdminCookieSecret == "" {
		log.Fatal("Error: admin cookie secret required (use -admin-cookie-secret or FAUCET_ADMIN_COOKIE_SECRET)")
	}
	if len(cfg.AdminCookieSecret) < 32 {
		log.Fatal("Error: admin cookie secret must be at least 32 characters")
	}
	if cfg.BitcoinRPC.User == "" {
		log.Fatal("Error: bitcoin RPC user required (use -bitcoin-rpc-user or FAUCET_BITCOIN_RPC_USER)")
	}
	if cfg.BitcoinRPC.Password == "" {
		log.Fatal("Error: bitcoin RPC password required (use -bitcoin-rpc-password or FAUCET_BITCOIN_RPC_PASSWORD)")
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
	log.Printf("Enabled amount ranges: %v (default: %d)", cfg.EnabledAmountRanges, cfg.DefaultAmountRange)
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

	if err := svc.CheckAndLoadBitcoinCoreWallet(); err != nil {
		log.Fatalf("Bitcoin RPC connection failed: %v", err)
	}
	log.Printf("Bitcoin RPC connection verified, wallet [%s] loaded", cfg.BitcoinCoreWalletName)

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
