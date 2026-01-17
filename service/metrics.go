package service

import (
	"fmt"
	"log"
	"net/http"
	"runtime"
	"time"

	"github.com/lnliz/faucet.coinbin.org/db"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	FaucetBuildInfo = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "faucet_build_info",
			Help: "Faucet build information",
		},
		[]string{"sha", "go_version"},
	)

	MetricFaucetTransactionCount = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "faucet_transactions_count",
			Help: "Number of transactions in db by status",
		},
		[]string{"status"},
	)

	FaucetWalletBalance = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "faucet_wallet_balance_btc",
			Help: "Current total wallet balance in BTC",
		},
	)

	FaucetTotalAmountSent = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "faucet_total_amount_sent_btc",
			Help: "Total amount sent in BTC",
		},
	)

	WalletUtxosCounts = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "faucet_wallet_utxos_count",
			Help: "faucet_wallet_utxos_countC",
		},
		[]string{"status"},
	)

	FaucetBitcoinHealthy = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "faucet_bitcoin_healthy",
			Help: "Bitcoin Core connection status (1=healthy, 0=unhealthy)",
		},
	)

	HttpRequestsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Total HTTP requests",
		},
		[]string{"method", "path", "status"},
	)

	HttpRequestDuration = promauto.NewSummaryVec(
		prometheus.SummaryOpts{
			Name:       "http_request_duration_seconds",
			Help:       "HTTP request duration in seconds",
			Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.95: 0.005, 0.99: 0.001},
		},
		[]string{"method", "path", "status"},
	)
)

func (svc *Service) CollectMetrics() {
	totalSentBTC := db.GetTotalAmountSentBTC(svc.db)
	FaucetTotalAmountSent.Set(totalSentBTC)

	for _, state := range []string{
		db.TxnStatusBroadcast,
		db.TxnStatusPending,
		db.TxnStatusFailed,
	} {
		c := db.GetTransactionCount(svc.db, state)
		MetricFaucetTransactionCount.WithLabelValues(state).Set(float64(c))
	}

	FaucetWalletBalance.Set(svc.GetAvailableWalletBalance())

	if utxos, err := svc.rpcClient.ListUnspent(0, 9999999); err == nil {
		countConfirmed := 0
		countPending := 0
		for _, u := range utxos {
			if u.Confirmations > 0 {
				countConfirmed++
			} else {
				countPending++
			}
		}
		WalletUtxosCounts.WithLabelValues("confirmed").Set(float64(countConfirmed))
		WalletUtxosCounts.WithLabelValues("pending").Set(float64(countPending))
	}

	_, err := svc.rpcClient.GetBlockchainInfo()
	if err != nil {
		FaucetBitcoinHealthy.Set(0)
	} else {
		FaucetBitcoinHealthy.Set(1)
	}
}

func (svc *Service) StartMetricsHttpServer() {
	FaucetBuildInfo.WithLabelValues(CommitHash, runtime.Version()).Set(1)

	go func() {
		http.Handle("/metrics", svc.MetricsHandler())
		log.Printf("Starting metrics server: http://%s/metrics", svc.cfg.MetricsAddr)
		if err := http.ListenAndServe(svc.cfg.MetricsAddr, nil); err != nil {
			log.Fatalf("Failed to start metrics server: %v", err)
		}
	}()
}

func (svc *Service) MetricsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		svc.CollectMetrics()
		promhttp.Handler().ServeHTTP(w, r)
	})
}

type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

func metricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rw := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rw, r)
		duration := time.Since(start).Seconds()
		status := fmt.Sprintf("%d", rw.statusCode)
		HttpRequestsTotal.WithLabelValues(r.Method, r.URL.Path, status).Inc()
		HttpRequestDuration.WithLabelValues(r.Method, r.URL.Path, status).Observe(duration)
	})
}
