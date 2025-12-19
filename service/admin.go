package service

import (
	"encoding/json"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/lnliz/faucet.coinbin.org/btc"
	"github.com/lnliz/faucet.coinbin.org/db"
)

const (
	adminSessionDurationHours = 4
)

func (svc *Service) adminLoginPageHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		data := map[string]interface{}{
			"Require2FA": svc.cfg.Admin2FASecret != "",
		}
		if err := svc.renderTemplate(w, "admin_login.html", data); err != nil {
			http.Error(w, "Internal server error", http.StatusInternalServerError)
		}
		return
	}

	if r.Method == http.MethodPost {
		svc.adminLoginHandler(w, r)
		return
	}

	http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
}

func (svc *Service) adminLoginHandler(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		data := map[string]interface{}{
			"Error":      "Invalid request",
			"Require2FA": svc.cfg.Admin2FASecret != "",
		}
		w.WriteHeader(http.StatusBadRequest)
		svc.renderTemplate(w, "admin_login.html", data)
		return
	}

	password := r.FormValue("password")
	totpCode := r.FormValue("totp_code")

	if password != svc.cfg.AdminPassword {
		data := map[string]interface{}{
			"Error":      "Invalid password",
			"Require2FA": svc.cfg.Admin2FASecret != "",
		}
		w.WriteHeader(http.StatusUnauthorized)
		svc.renderTemplate(w, "admin_login.html", data)
		return
	}

	if svc.cfg.Admin2FASecret != "" {
		if totpCode == "" {
			data := map[string]interface{}{
				"Error":      "2FA code required",
				"Require2FA": true,
			}
			w.WriteHeader(http.StatusUnauthorized)
			svc.renderTemplate(w, "admin_login.html", data)
			return
		}

		if !svc.totp.Verify(totpCode, time.Now().Unix()) {
			data := map[string]interface{}{
				"Error":      "Invalid 2FA code",
				"Require2FA": true,
			}
			w.WriteHeader(http.StatusUnauthorized)
			svc.renderTemplate(w, "admin_login.html", data)
			return
		}
	}

	sessionID := uuid.New().String()
	expiresAt := time.Now().Add(time.Duration(adminSessionDurationHours) * time.Hour)

	session := db.AdminSession{
		SessionID: sessionID,
		IPAddress: svc.getClientIP(r),
		UserAgent: r.UserAgent(),
		ExpiresAt: expiresAt,
	}

	if err := svc.db.Create(&session).Error; err != nil {
		log.Printf("Failed to create admin session: %v", err)
		data := map[string]interface{}{
			"Error": "Failed to create session",
		}
		w.WriteHeader(http.StatusInternalServerError)
		svc.renderTemplate(w, "admin_login.html", data)
		return
	}

	signedCookie := svc.signCookie(sessionID)
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    signedCookie,
		Path:     svc.cfg.AdminPath,
		MaxAge:   adminSessionDurationHours * 60 * 60,
		HttpOnly: true,
	})

	http.Redirect(w, r, svc.cfg.AdminPath+"/", http.StatusFound)
}

func (svc *Service) adminLogoutHandler(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("admin_session")
	if err == nil {
		sessionID, valid := svc.validateSessionCookie(cookie.Value)
		if valid {
			svc.db.Where("session_id = ?", sessionID).Delete(&db.AdminSession{})
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "admin_session",
		Value:    "",
		Path:     svc.cfg.AdminPath,
		MaxAge:   -1,
		HttpOnly: true,
	})

	http.Redirect(w, r, svc.cfg.AdminPath+"/login", http.StatusFound)
}

func (svc *Service) adminDashboardHandler(w http.ResponseWriter, r *http.Request) {
	balances, err := svc.rpcClient.GetBalances()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	totalSent := db.GetTransactionCount(svc.db, db.TxnStatusBroadcast)
	totalPending := db.GetTransactionCount(svc.db, db.TxnStatusPending)
	totalFailed := db.GetTransactionCount(svc.db, db.TxnStatusFailed)

	totalAmount := db.GetTotalAmountSentBTC(svc.db)

	transactions, err := db.GetTransactions(svc.db, "", "created_at DESC", 50)
	if err != nil {
		log.Printf("Failed to get transactions: %v", err)
	}

	data := map[string]interface{}{
		"BalanceTrusted":                  balances.Mine.Trusted,
		"BalancePending":                  balances.Mine.Untrusted,
		"BalanceImmature":                 balances.Mine.Immature,
		"BalanceTotal":                    balances.Mine.Trusted + balances.Mine.Untrusted + balances.Mine.Immature,
		"TotalSent":                       totalSent,
		"TotalPending":                    totalPending,
		"TotalFailed":                     totalFailed,
		"TotalAmount":                     totalAmount,
		"Transactions":                    transactions,
		"AdminPath":                       svc.cfg.AdminPath,
		"Require2FA":                      svc.cfg.Admin2FASecret != "",
		"CommitHash":                      CommitHash,
		"ConsolidationAmountThresholdBTC": svc.cfg.ConsolidationAmountThresholdBTC,
		"MaxConsolidationUTXOs":           svc.cfg.MaxConsolidationUTXOs,
		"MinConsolidationUTXOs":           svc.cfg.MinConsolidationUTXOs,
		"AutoConsolidationInterval":       svc.cfg.AutoConsolidationInterval,
	}

	if err := svc.renderTemplate(w, "admin_dashboard.html", data); err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
	}
}

func (svc *Service) adminGetBalanceHandler(w http.ResponseWriter, r *http.Request) {
	balances, err := svc.rpcClient.GetBalances()
	if err != nil {
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"trusted":  balances.Mine.Trusted,
		"pending":  balances.Mine.Untrusted,
		"immature": balances.Mine.Immature,
		"total":    balances.Mine.Trusted + balances.Mine.Untrusted + balances.Mine.Immature,
	})
}

func (svc *Service) adminGetNewAddressHandler(w http.ResponseWriter, r *http.Request) {
	address, err := svc.rpcClient.GetNewAddress("", "bech32")
	if err != nil {
		log.Printf("Failed to generate new address: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to generate address"})
		return
	}

	log.Printf("Generated new deposit address: %s", address)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"address": address})
}

func (svc *Service) adminSendFundsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Address   string  `json:"address"`
		AmountBTC float64 `json:"amount"`
		TOTPCode  string  `json:"totp_code"`
		OpReturn  string  `json:"op_return"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request"})
		return
	}

	if svc.cfg.Admin2FASecret != "" {
		if req.TOTPCode == "" || !svc.totp.Verify(req.TOTPCode, time.Now().Unix()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid 2FA code"})
			return
		}
	}

	if err := btc.ValidateSignetAddress(req.Address); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if req.AmountBTC <= 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Amount must be greater than 0"})
		return
	}

	availBalance := svc.GetAvailableWalletBalance()
	if req.AmountBTC > availBalance {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Insufficient balance"})
		return
	}

	fees := btc.FeeSatsPerVBLowerLimit * 1.10

	txid, err := svc.rpcClient.SendToAddressWithOpReturn(
		req.Address,
		req.AmountBTC,
		fees,
		req.OpReturn,
	)

	if err != nil {
		log.Printf("Admin send failed: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to send transaction"})
		return
	}

	log.Printf("Admin sent %.8f BTC to %s (txid: %s)", req.AmountBTC, req.Address, txid)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"txid":    txid,
		"message": "Transaction sent successfully",
	})
}

func (svc *Service) adminGetUTXOsHandler(w http.ResponseWriter, r *http.Request) {
	utxos, err := svc.rpcClient.ListUnspent(0, 9999999)
	if err != nil {
		log.Printf("Failed to list UTXOs: %v", err)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": "Failed to list UTXOs"})
		return
	}

	sort.Slice(utxos, func(i, j int) bool {
		if utxos[i].Confirmations != utxos[j].Confirmations {
			return utxos[i].Confirmations < utxos[j].Confirmations
		}
		if utxos[i].Address == utxos[j].Address {
			return utxos[i].Address < utxos[j].Address
		}
		return utxos[i].Vout < utxos[j].Vout
	})

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"utxos": utxos,
	})
}

func (svc *Service) adminConsolidateUTXOsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TOTPCode string `json:"totp_code"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "Invalid request"})
		return
	}

	if svc.cfg.Admin2FASecret != "" {
		if req.TOTPCode == "" || !svc.totp.Verify(req.TOTPCode, time.Now().Unix()) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "Invalid 2FA code"})
			return
		}
	}

	result, err := svc.ConsolidateUTXOs()

	w.Header().Set("Content-Type", "application/json")

	if err != nil {
		log.Printf("Failed to consolidate UTXOs: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}

	if result.SkipReason != "" {
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"message": result.SkipReason,
			"count":   result.Count,
		})
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"txid":    result.TxID,
		"count":   result.Count,
		"amount":  result.Amount,
		"address": result.Address,
		"message": result.Message,
	})
}
