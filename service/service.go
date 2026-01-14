package service

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lnliz/faucet.coinbin.org/btc"
	"github.com/lnliz/faucet.coinbin.org/db"
	"github.com/lnliz/go-turnstile"
	"github.com/xlzd/gotp"
	"gorm.io/gorm"
)

type Config struct {
	ListenAddr                      string
	MetricsAddr                     string
	DataDir                         string
	BitcoinRPC                      btc.BitcoinRPCConfig
	BatchInterval                   time.Duration
	MinAmountBTC                    float64
	MaxAmountBTC                    float64
	MinBalance                      float64
	TurnstileSecret                 string
	TurnstileSiteKey                string
	AdminPassword                   string
	AdminPath                       string
	AdminCookieSecret               string
	AdminIPAllowlist                []string
	Admin2FASecret                  string
	ConsolidationAmountThresholdBTC float64
	MaxConsolidationUTXOs           int
	MinConsolidationUTXOs           int
	MaxWithdrawalsPerIP24h          int
	AutoConsolidationInterval       time.Duration
}

type Service struct {
	cfg       *Config
	db        *gorm.DB
	turnstile *turnstile.TurnstileVerifier
	totp      *gotp.TOTP

	walletBalance    float64
	walletBalanceMtx sync.RWMutex

	rpcClient *btc.BitcoinRPCClient
}

const (
	walletName = "faucet"
)

var (
	CommitHash = "<<dev>>"
)

func NewService(cfg *Config, database *gorm.DB) *Service {
	rpcClient := btc.NewBitcoinRPCClient(&cfg.BitcoinRPC)

	t := turnstile.NewTurnstileVerifier(cfg.TurnstileSecret)
	t.HttpClient = &http.Client{Timeout: 2 * time.Second}

	return &Service{
		cfg:       cfg,
		db:        database,
		turnstile: t,
		totp:      gotp.NewDefaultTOTP(strings.ToUpper(strings.TrimSpace(cfg.Admin2FASecret))),

		rpcClient: rpcClient.WithWallet(walletName),
	}
}

func (svc *Service) renderTemplate(w http.ResponseWriter, templateName string, data interface{}) error {
	tmpl, err := template.ParseGlob("templates/*.html")
	if err != nil {
		log.Printf("Failed to parse templates: %v", err)
		return err
	}

	if err := tmpl.ExecuteTemplate(w, templateName, data); err != nil {
		log.Printf("Failed to render template %s: %v", templateName, err)
		return err
	}

	return nil
}

func (svc *Service) CheckBitcoinConnection() error {
	wallets, err := svc.rpcClient.ListWallets()
	if err != nil {
		return fmt.Errorf("failed to list wallets: %w", err)
	}

	faucetWalletFound := false
	for _, wallet := range wallets {
		if wallet == walletName {
			faucetWalletFound = true
			break
		}
	}

	if !faucetWalletFound {
		log.Printf("'%s' wallet not loaded, attempting to load it...", walletName)
		if err := svc.rpcClient.LoadWallet(walletName); err != nil {
			return fmt.Errorf("'%s' wallet not found or failed to load - please create it with: bitcoin-cli -signet createwallet %s (error: %v)", walletName, walletName, err)
		}
		log.Printf("'%s' wallet loaded successfully", walletName)
	}

	log.Println("Bitcoin RPC connection verified, 'faucet' wallet loaded")
	return nil
}

func (svc *Service) adminIPAllowlistMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientIP := svc.getClientIP(r)

		allowed := false
		for _, allowedIP := range svc.cfg.AdminIPAllowlist {
			if clientIP == allowedIP {
				allowed = true
				break
			}
		}

		if !allowed {
			log.Printf("Admin - denied access, [ip=%s] [path=%s]", clientIP, r.URL.Path)
			http.Error(w, "Access denied", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (svc *Service) adminAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("admin_session")
		if err != nil {
			http.Redirect(w, r, svc.cfg.AdminPath+"/login", http.StatusFound)
			return
		}

		sessionID, valid := svc.validateSessionCookie(cookie.Value)
		if !valid {
			http.Redirect(w, r, svc.cfg.AdminPath+"/login", http.StatusFound)
			return
		}

		var session db.AdminSession
		if err := svc.db.Where("session_id = ? AND expires_at > ?", sessionID, time.Now()).First(&session).Error; err != nil {
			http.Redirect(w, r, svc.cfg.AdminPath+"/login", http.StatusFound)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (svc *Service) signCookie(value string) string {
	h := hmac.New(sha256.New, []byte(svc.cfg.AdminCookieSecret))
	h.Write([]byte(value))
	signature := hex.EncodeToString(h.Sum(nil))
	return value + "." + signature
}

func (svc *Service) validateSessionCookie(cookie string) (string, bool) {
	parts := strings.Split(cookie, ".")
	if len(parts) != 2 {
		return "", false
	}

	sessionID := parts[0]
	providedSig := parts[1]

	h := hmac.New(sha256.New, []byte(svc.cfg.AdminCookieSecret))
	h.Write([]byte(sessionID))
	expectedSig := hex.EncodeToString(h.Sum(nil))

	if !hmac.Equal([]byte(providedSig), []byte(expectedSig)) {
		return "", false
	}

	return sessionID, true
}

func (svc *Service) GetAvailableWalletBalance() float64 {
	balances, err := svc.rpcClient.GetBalances()
	if err != nil {
		log.Printf("Failed to get balances: %v", err)
		return 0.0
	}
	return balances.Mine.Trusted + balances.Mine.Untrusted
}

func (svc *Service) getClientIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		ips := strings.Split(xff, ",")
		if len(ips) > 0 {
			return strings.TrimSpace(ips[0])
		}
	}

	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return xri
	}

	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

func (svc *Service) StartService() *http.Server {
	mux := http.NewServeMux()

	mux.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	mux.HandleFunc("/", svc.indexHandler)
	mux.HandleFunc("/api/submit", svc.submitHandler)
	mux.HandleFunc("/health", svc.healthHandler)

	adminMux := http.NewServeMux()
	adminMux.HandleFunc(svc.cfg.AdminPath+"/login", svc.adminLoginPageHandler)
	adminMux.Handle(svc.cfg.AdminPath+"/", svc.adminAuthMiddleware(http.HandlerFunc(svc.adminDashboardHandler)))
	adminMux.Handle(svc.cfg.AdminPath+"/logout", svc.adminAuthMiddleware(http.HandlerFunc(svc.adminLogoutHandler)))
	adminMux.Handle(svc.cfg.AdminPath+"/balance", svc.adminAuthMiddleware(http.HandlerFunc(svc.adminGetBalanceHandler)))
	adminMux.Handle(svc.cfg.AdminPath+"/getnewaddress", svc.adminAuthMiddleware(http.HandlerFunc(svc.adminGetNewAddressHandler)))
	adminMux.Handle(svc.cfg.AdminPath+"/sendfunds", svc.adminAuthMiddleware(http.HandlerFunc(svc.adminSendFundsHandler)))
	adminMux.Handle(svc.cfg.AdminPath+"/utxos", svc.adminAuthMiddleware(http.HandlerFunc(svc.adminGetUTXOsHandler)))
	adminMux.Handle(svc.cfg.AdminPath+"/consolidate", svc.adminAuthMiddleware(http.HandlerFunc(svc.adminConsolidateUTXOsHandler)))

	finalMux := http.NewServeMux()
	finalMux.Handle("/", mux)
	finalMux.Handle(svc.cfg.AdminPath+"/", svc.adminIPAllowlistMiddleware(adminMux))

	server := &http.Server{
		Addr:    svc.cfg.ListenAddr,
		Handler: metricsMiddleware(finalMux),
	}

	log.Printf("Starting HTTP server on http://%s", svc.cfg.ListenAddr)
	log.Printf("Admin dashboard: http://%s%s", svc.cfg.ListenAddr, svc.cfg.AdminPath)

	return server
}

func (svc *Service) StartBalanceRefresher(ctx context.Context, wg *sync.WaitGroup) {
	interval := 5 * time.Minute
	log.Printf("Starting balance refresher with interval: %s", interval)

	// init once so balance is not empty
	svc.walletBalance = svc.GetAvailableWalletBalance()

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Println("Balance refresher received shutdown signal")
				return
			case <-ticker.C:
				bal := svc.GetAvailableWalletBalance()
				if bal > 0 {
					svc.walletBalanceMtx.Lock()
					svc.walletBalance = bal
					svc.walletBalanceMtx.Unlock()
				}
			}
		}
	}()
}

func (svc *Service) GetCachedWalletBalance() float64 {
	svc.walletBalanceMtx.RLock()
	defer svc.walletBalanceMtx.RUnlock()
	return svc.walletBalance
}
