package db

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}
	if err := db.AutoMigrate(&Transaction{}, &AdminSession{}); err != nil {
		t.Fatalf("failed to migrate: %v", err)
	}
	return db
}

func seedTransactions(t *testing.T, db *gorm.DB, txns []Transaction) {
	t.Helper()
	for i := range txns {
		if err := db.Create(&txns[i]).Error; err != nil {
			t.Fatalf("failed to seed transaction: %v", err)
		}
	}
}

func TestInitDB(t *testing.T) {
	dir := t.TempDir()
	database, err := InitDB(dir)
	if err != nil {
		t.Fatalf("InitDB failed: %v", err)
	}
	if database == nil {
		t.Fatal("expected non-nil db")
	}

	if err := database.First(&Transaction{}).Error; err != gorm.ErrRecordNotFound {
		t.Errorf("expected ErrRecordNotFound for empty table, got: %v", err)
	}
	if err := database.First(&AdminSession{}).Error; err != gorm.ErrRecordNotFound {
		t.Errorf("expected ErrRecordNotFound for empty table, got: %v", err)
	}
}

func TestInitDB_InvalidPath(t *testing.T) {
	_, err := InitDB("/dev/null/impossible")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestGetTransactionCount(t *testing.T) {
	db := setupTestDB(t)

	if got := GetTransactionCount(db, TxnStatusPending); got != 0 {
		t.Errorf("expected 0, got %d", got)
	}

	seedTransactions(t, db, []Transaction{
		{Address: "a1", Status: TxnStatusPending, AmountBTC: 0.01},
		{Address: "a2", Status: TxnStatusPending, AmountBTC: 0.02},
		{Address: "a3", Status: TxnStatusBroadcast, AmountBTC: 0.03},
		{Address: "a4", Status: TxnStatusFailed, AmountBTC: 0.04},
	})

	tests := []struct {
		status string
		want   int64
	}{
		{TxnStatusPending, 2},
		{TxnStatusBroadcast, 1},
		{TxnStatusFailed, 1},
		{TxnStatusProcessing, 0},
	}

	for _, tt := range tests {
		if got := GetTransactionCount(db, tt.status); got != tt.want {
			t.Errorf("GetTransactionCount(%q) = %d, want %d", tt.status, got, tt.want)
		}
	}
}

func TestGetTotalAmountSentBTC(t *testing.T) {
	db := setupTestDB(t)

	if got := GetTotalAmountSentBTC(db); got != 0 {
		t.Errorf("expected 0 for empty db, got %f", got)
	}

	seedTransactions(t, db, []Transaction{
		{Address: "a1", Status: TxnStatusBroadcast, AmountBTC: 0.5},
		{Address: "a2", Status: TxnStatusBroadcast, AmountBTC: 1.5},
		{Address: "a3", Status: TxnStatusPending, AmountBTC: 99.0},
		{Address: "a4", Status: TxnStatusFailed, AmountBTC: 50.0},
	})

	got := GetTotalAmountSentBTC(db)
	want := 2.0
	if got != want {
		t.Errorf("GetTotalAmountSentBTC = %f, want %f", got, want)
	}
}

func TestGetTransactions_NoFilter(t *testing.T) {
	db := setupTestDB(t)
	seedTransactions(t, db, []Transaction{
		{Address: "a1", Status: TxnStatusPending},
		{Address: "a2", Status: TxnStatusBroadcast},
		{Address: "a3", Status: TxnStatusFailed},
	})

	txns, err := GetTransactions(db, "", "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 3 {
		t.Errorf("expected 3 transactions, got %d", len(txns))
	}
}

func TestGetTransactions_StatusFilter(t *testing.T) {
	db := setupTestDB(t)
	seedTransactions(t, db, []Transaction{
		{Address: "a1", Status: TxnStatusPending},
		{Address: "a2", Status: TxnStatusPending},
		{Address: "a3", Status: TxnStatusBroadcast},
	})

	txns, err := GetTransactions(db, TxnStatusPending, "", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Errorf("expected 2 pending, got %d", len(txns))
	}
}

func TestGetTransactions_Order(t *testing.T) {
	db := setupTestDB(t)
	seedTransactions(t, db, []Transaction{
		{Address: "a1", Status: TxnStatusPending, AmountBTC: 0.01},
		{Address: "a2", Status: TxnStatusPending, AmountBTC: 0.09},
		{Address: "a3", Status: TxnStatusPending, AmountBTC: 0.05},
	})

	txns, err := GetTransactions(db, "", "amount_btc DESC", 0)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if txns[0].AmountBTC != 0.09 {
		t.Errorf("expected first txn amount 0.09, got %f", txns[0].AmountBTC)
	}
}

func TestGetTransactions_Limit(t *testing.T) {
	db := setupTestDB(t)
	seedTransactions(t, db, []Transaction{
		{Address: "a1", Status: TxnStatusPending},
		{Address: "a2", Status: TxnStatusPending},
		{Address: "a3", Status: TxnStatusPending},
	})

	txns, err := GetTransactions(db, "", "", 2)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(txns) != 2 {
		t.Errorf("expected 2 transactions, got %d", len(txns))
	}
}

func TestTransaction_UpdateStatus(t *testing.T) {
	db := setupTestDB(t)

	tx := Transaction{Address: "a1", Status: TxnStatusPending, AmountBTC: 0.01}
	if err := db.Create(&tx).Error; err != nil {
		t.Fatalf("failed to create: %v", err)
	}

	if err := tx.UpdateStatus(db, TxnStatusProcessing); err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	var reloaded Transaction
	db.First(&reloaded, tx.ID)
	if reloaded.Status != TxnStatusProcessing {
		t.Errorf("expected status %q, got %q", TxnStatusProcessing, reloaded.Status)
	}

	if err := tx.UpdateStatus(db, TxnStatusBroadcast); err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}
	db.First(&reloaded, tx.ID)
	if reloaded.Status != TxnStatusBroadcast {
		t.Errorf("expected status %q, got %q", TxnStatusBroadcast, reloaded.Status)
	}
}

func TestAdminSession_CRUD(t *testing.T) {
	db := setupTestDB(t)

	session := AdminSession{
		SessionID: "sess-abc-123",
		IPAddress: "10.0.0.1",
		UserAgent: "TestAgent/1.0",
		ExpiresAt: time.Now().Add(4 * time.Hour),
	}
	if err := db.Create(&session).Error; err != nil {
		t.Fatalf("failed to create session: %v", err)
	}

	var loaded AdminSession
	if err := db.Where("session_id = ?", "sess-abc-123").First(&loaded).Error; err != nil {
		t.Fatalf("failed to find session: %v", err)
	}
	if loaded.IPAddress != "10.0.0.1" {
		t.Errorf("expected IP 10.0.0.1, got %s", loaded.IPAddress)
	}

	dupe := AdminSession{
		SessionID: "sess-abc-123",
		IPAddress: "10.0.0.2",
		ExpiresAt: time.Now().Add(1 * time.Hour),
	}
	if err := db.Create(&dupe).Error; err == nil {
		t.Error("expected unique constraint violation for duplicate session_id")
	}

	if err := db.Where("session_id = ?", "sess-abc-123").Delete(&AdminSession{}).Error; err != nil {
		t.Fatalf("failed to delete session: %v", err)
	}
	err := db.Where("session_id = ?", "sess-abc-123").First(&AdminSession{}).Error
	if err != gorm.ErrRecordNotFound {
		t.Errorf("expected ErrRecordNotFound after delete, got: %v", err)
	}
}

func TestAdminSession_Expiry(t *testing.T) {
	db := setupTestDB(t)

	expired := AdminSession{
		SessionID: "expired-sess",
		IPAddress: "10.0.0.1",
		ExpiresAt: time.Now().Add(-1 * time.Hour),
	}
	active := AdminSession{
		SessionID: "active-sess",
		IPAddress: "10.0.0.2",
		ExpiresAt: time.Now().Add(4 * time.Hour),
	}
	db.Create(&expired)
	db.Create(&active)

	var activeSessions []AdminSession
	db.Where("expires_at > ?", time.Now()).Find(&activeSessions)
	if len(activeSessions) != 1 {
		t.Errorf("expected 1 active session, got %d", len(activeSessions))
	}
	if activeSessions[0].SessionID != "active-sess" {
		t.Errorf("expected active-sess, got %s", activeSessions[0].SessionID)
	}
}

func TestTransaction_IPAddressIndexQuery(t *testing.T) {
	db := setupTestDB(t)
	seedTransactions(t, db, []Transaction{
		{Address: "a1", IPAddress: "1.2.3.4", Status: TxnStatusPending},
		{Address: "a2", IPAddress: "1.2.3.4", Status: TxnStatusBroadcast},
		{Address: "a3", IPAddress: "5.6.7.8", Status: TxnStatusPending},
	})

	var count int64
	db.Model(&Transaction{}).Where("ip_address = ?", "1.2.3.4").Count(&count)
	if count != 2 {
		t.Errorf("expected 2 transactions for IP 1.2.3.4, got %d", count)
	}
}

func TestTransaction_AddressQuery(t *testing.T) {
	db := setupTestDB(t)
	seedTransactions(t, db, []Transaction{
		{Address: "tb1qaddr1", IPAddress: "1.1.1.1", Status: TxnStatusBroadcast},
		{Address: "tb1qaddr1", IPAddress: "2.2.2.2", Status: TxnStatusPending},
		{Address: "tb1qaddr2", IPAddress: "3.3.3.3", Status: TxnStatusBroadcast},
	})

	var count int64
	db.Model(&Transaction{}).Where("address = ?", "tb1qaddr1").Count(&count)
	if count != 2 {
		t.Errorf("expected 2 transactions for address tb1qaddr1, got %d", count)
	}
}
