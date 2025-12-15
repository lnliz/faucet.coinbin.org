package service

import (
	"log"
	"net/http"

	"github.com/lnliz/faucet.coinbin.org/db"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
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
	go func() {
		http.Handle("/metrics", svc.MetricsHandler())
		log.Printf("Starting metrics server on http://%s", svc.cfg.MetricsAddr)
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
