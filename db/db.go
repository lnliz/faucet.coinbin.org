package db

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type Transaction struct {
	ID           uint      `gorm:"primaryKey"`
	CreatedAt    time.Time `gorm:"index"`
	Address      string    `gorm:"uniqueIndex;not null"`
	IPAddress    string    `gorm:"index"`
	OnchainTxnID string    `gorm:"column:onchain_txn_id;index"`
	AmountBTC    float64   `gorm:"not null;default:0"`
	Status       string    `gorm:"index;not null"`
	ErrorMsg     string    `gorm:"type:text"`
}

const (
	TxnStatusPending    = "pending"
	TxnStatusProcessing = "processing"
	TxnStatusFailed     = "failed"
	TxnStatusBroadcast  = "broadcast"
)

type AdminSession struct {
	ID        uint   `gorm:"primaryKey"`
	SessionID string `gorm:"uniqueIndex;not null"`
	IPAddress string `gorm:"not null"`
	UserAgent string `gorm:"type:text"`
	CreatedAt time.Time
	ExpiresAt time.Time `gorm:"index"`
}

func InitDB(dataDir string) (*gorm.DB, error) {
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		return nil, err
	}

	dbPath := filepath.Join(dataDir, "faucet.db")
	log.Printf("Using database: %s", dbPath)

	db, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{})
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(&Transaction{}, &AdminSession{}); err != nil {
		return nil, err
	}

	return db, nil
}

func GetTransactionCount(db *gorm.DB, status string) int64 {
	var count int64
	db.Model(&Transaction{}).Where("status = ?", status).Count(&count)
	return count
}

func GetTotalAmountSentBTC(db *gorm.DB) float64 {
	var totalAmount float64
	db.Model(&Transaction{}).Where("status = ?", TxnStatusBroadcast).Select("COALESCE(SUM(amount_btc), 0)").Row().Scan(&totalAmount)
	return totalAmount
}

func GetTransactions(db *gorm.DB, status string, order string, limit int) ([]Transaction, error) {
	q := db
	if status != "" {
		q = q.Where("status = ?", status)
	}

	if order != "" {
		q = q.Order(order)
	}

	if limit > 0 {
		q = q.Limit(limit)
	}

	var result []Transaction
	if err := q.Find(&result).Error; err != nil {
		log.Printf("Failed to query transactions: %v", err)
		return nil, err
	}

	return result, nil
}

func (tx *Transaction) UpdateStatus(db *gorm.DB, newStatus string) error {
	return db.Model(&tx).Update("status", newStatus).Error
}
