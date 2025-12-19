package service

import (
	"encoding/json"
	"log"
	"math/rand"
	"net/http"
	"time"

	"github.com/lnliz/faucet.coinbin.org/btc"
	"github.com/lnliz/faucet.coinbin.org/db"
)

func (svc *Service) indexHandler(w http.ResponseWriter, r *http.Request) {
	data := map[string]interface{}{
		"TurnstileSiteKey": svc.cfg.TurnstileSiteKey,
		"CommitHash":       CommitHash,
		"WalletBalance":    svc.GetCachedWalletBalance(),
	}
	if err := svc.renderTemplate(w, "index.html", data); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func (svc *Service) submitHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Address        string `json:"address"`
		TurnstileToken string `json:"turnstile_token"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request"})
		return
	}

	clientIP := svc.getClientIP(r)

	if svc.cfg.TurnstileSecret != "" {
		if req.TurnstileToken == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Turnstile verification required"})
			return
		}

		resp, err := svc.turnstile.Verify(req.TurnstileToken)
		if err != nil {
			log.Printf("Turnstile verification error: %v", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Verification failed"})
			return
		}

		if !resp.Success {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Turnstile verification failed"})
			return
		}
	}

	if err := btc.ValidateSignetAddress(req.Address); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	isAdminIP := false
	for _, allowedIP := range svc.cfg.AdminIPAllowlist {
		if clientIP == allowedIP {
			isAdminIP = true
			break
		}
	}

	if !isAdminIP {
		var count int64
		cutoff := time.Now().Add(-24 * time.Hour)

		if err := svc.db.Model(&db.Transaction{}).
			Where("ip_address = ? AND created_at > ?", clientIP, cutoff).
			Count(&count).Error; err != nil {

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": "Internal error"})
			return
		}

		if count >= 2 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(map[string]string{"error": "Rate limit exceeded (max 2 per 24h)"})
			return
		}
	}

	rangeSats := int((svc.cfg.MaxAmountBTC - svc.cfg.MinAmountBTC) * 100_000_000)
	randSats := rand.Intn(rangeSats)
	amountBTC := svc.cfg.MinAmountBTC + 0.00000001*float64(randSats)

	tx := db.Transaction{
		Address:   req.Address,
		IPAddress: clientIP,
		AmountBTC: amountBTC,
		Status:    db.TxnStatusPending,
	}

	if err := svc.db.Create(&tx).Error; err != nil {
		if err.Error() == "UNIQUE constraint failed: transactions.address" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			json.NewEncoder(w).Encode(map[string]string{"error": "Address already used"})
			return
		}
		log.Printf("Failed to create transaction: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to queue address"})
		return
	}

	log.Printf("Address queued: %s (IP: %s)", req.Address, clientIP)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Address queued, coins are on the way!",
	})
}

func (svc *Service) healthHandler(w http.ResponseWriter, r *http.Request) {
	if _, err := svc.rpcClient.GetBlockchainInfo(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unhealthy"))
		return
	}

	if err := svc.db.Exec("SELECT 1").Error; err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("unhealthy"))
		return
	}

	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}
