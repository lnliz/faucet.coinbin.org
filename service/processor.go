package service

import (
	"context"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/lnliz/faucet.coinbin.org/db"
)

const (
	defaultOpReturn = "<3 faucet.coinbin.org <3"
)

func (svc *Service) StartBatchProcessor(ctx context.Context, wg *sync.WaitGroup) {
	log.Printf("Starting batch processor with interval: %s", svc.cfg.BatchInterval)

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(svc.cfg.BatchInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Println("Batch processor received shutdown signal, finishing current work...")
				return
			case <-ticker.C:
				svc.processBatch()
			}
		}
	}()
}

func (svc *Service) processBatch() {
	pendingTxns, err := db.GetTransactions(svc.db, db.TxnStatusPending, "", 50)
	if err != nil {
		log.Printf("Failed to query pending transactions: %v", err)
		return
	}

	if len(pendingTxns) == 0 {
		return
	}

	log.Printf("Processing batch of %d transactions", len(pendingTxns))

	totalNeededBTC := 0.0
	for _, tx := range pendingTxns {
		totalNeededBTC += tx.AmountBTC
	}

	availableBalance := svc.GetAvailableWalletBalance()
	if availableBalance < totalNeededBTC {
		log.Printf("Insufficient balance: %.8f BTC available - need %.8f BTC for %d transactions",
			availableBalance, totalNeededBTC, len(pendingTxns))
		return
	}

	sent := 0
	failed := 0

	for _, tx := range pendingTxns {
		if err := tx.UpdateStatus(svc.db, db.TxnStatusProcessing); err != nil {
			log.Printf("Failed to update transaction %d to processing: %v", tx.ID, err)
			continue
		}

		fees := feeSatsPerVBLowerLimit * 1.15
		txid, err := svc.rpcClient.SendToAddressWithOpReturn(
			tx.Address,
			tx.AmountBTC,
			fees,
			defaultOpReturn,
		)

		if err != nil {
			log.Printf("Failed to send to %s: %v", tx.Address, err)
			if err := svc.db.Model(&tx).Updates(map[string]interface{}{
				"status":    db.TxnStatusFailed,
				"error_msg": err.Error(),
			}).Error; err != nil {
				log.Printf("Failed to update transaction %d to failed: %v", tx.ID, err)
			}
			failed++
			continue
		}

		if err := svc.db.Model(&tx).Updates(map[string]interface{}{
			"status":         db.TxnStatusBroadcast,
			"onchain_txn_id": txid,
		}).Error; err != nil {
			log.Printf("Failed to update transaction %d to sent: %v", tx.ID, err)
		}

		log.Printf("Sent %.8f BTC to %s (txid: %s)", tx.AmountBTC, tx.Address, txid)
		sent++
	}

	log.Printf("Batch complete: %d sent, %d failed", sent, failed)
}

type ConsolidationResult struct {
	TxID       string
	Count      int
	Amount     float64
	Address    string
	Message    string
	SkipReason string
}

func (svc *Service) ConsolidateUTXOs() (*ConsolidationResult, error) {
	utxos, err := svc.rpcClient.ListUnspent(0, 9999999)
	if err != nil {
		return nil, fmt.Errorf("failed to list UTXOs: %w", err)
	}

	sort.Slice(utxos, func(i, j int) bool {
		return utxos[i].Amount < utxos[j].Amount
	})

	var smallUTXOs []UTXO
	var totalAmount float64
	for _, utxo := range utxos {
		if utxo.Amount > svc.cfg.ConsolidationAmountThresholdBTC || !utxo.Spendable {
			continue
		}

		if utxo.Amount < dustLimitBTC {
			continue
		}

		if len(smallUTXOs) >= svc.cfg.MaxConsolidationUTXOs {
			break
		}

		smallUTXOs = append(smallUTXOs, utxo)
		totalAmount += utxo.Amount
	}

	if len(smallUTXOs) == 0 {
		return &ConsolidationResult{
			SkipReason: fmt.Sprintf("No UTXOs smaller than %.8f BTC to consolidate", svc.cfg.ConsolidationAmountThresholdBTC),
		}, nil
	}

	if len(smallUTXOs) < svc.cfg.MinConsolidationUTXOs {
		return &ConsolidationResult{
			Count:      len(smallUTXOs),
			SkipReason: fmt.Sprintf("Found %d small UTXOs, need at least %d to consolidate", len(smallUTXOs), svc.cfg.MinConsolidationUTXOs),
		}, nil
	}

	newAddress, err := svc.rpcClient.GetNewAddress("consolidated", "bech32")
	if err != nil {
		return nil, fmt.Errorf("failed to generate new address: %w", err)
	}

	txid, err := svc.rpcClient.Consolidate(
		smallUTXOs,
		totalAmount,
		newAddress,
		defaultOpReturn,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to consolidate: %w", err)
	}

	return &ConsolidationResult{
		TxID:    txid,
		Count:   len(smallUTXOs),
		Amount:  totalAmount,
		Address: newAddress,
		Message: fmt.Sprintf("Consolidated %d UTXOs (%.8f BTC)", len(smallUTXOs), totalAmount),
	}, nil
}

func (svc *Service) StartAutoConsolidation(ctx context.Context, wg *sync.WaitGroup) {
	log.Printf("Starting auto-consolidation with interval: %s", svc.cfg.AutoConsolidationInterval)

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(svc.cfg.AutoConsolidationInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				log.Println("Auto-consolidation received shutdown signal")
				return
			case <-ticker.C:
				result, err := svc.ConsolidateUTXOs()
				if err != nil {
					log.Printf("Auto-consolidation failed: %v", err)
					return
				}
				log.Printf("Consolidation result: %#v", result)
			}
		}
	}()
}
