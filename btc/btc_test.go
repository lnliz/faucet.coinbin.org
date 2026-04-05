package btc

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// mock RPC server
// ---------------------------------------------------------------------------

type mockRPCReq struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     string          `json:"id"`
}

type mockRPCErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mockRPC struct {
	handlers    map[string]func(json.RawMessage) (any, *mockRPCErr)
	lastMethod  string
	lastParams  json.RawMessage
	lastPath    string
	lastAuthOK  bool
	callCount   int
	methodCalls map[string]int
}

func newMockRPC() *mockRPC {
	return &mockRPC{
		handlers:    make(map[string]func(json.RawMessage) (any, *mockRPCErr)),
		methodCalls: make(map[string]int),
	}
}

func (m *mockRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.lastPath = r.URL.Path
	m.callCount++

	user, pass, ok := r.BasicAuth()
	m.lastAuthOK = ok && user != "" && pass != ""

	body, _ := io.ReadAll(r.Body)
	var req mockRPCReq
	json.Unmarshal(body, &req)
	m.lastMethod = req.Method
	m.lastParams = req.Params
	m.methodCalls[req.Method]++

	handler, ok := m.handlers[req.Method]
	if !ok {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"result": nil,
			"error":  &mockRPCErr{Code: -32601, Message: "Method not found: " + req.Method},
			"id":     req.ID,
		})
		return
	}

	result, rpcErr := handler(req.Params)
	resp := map[string]any{"id": req.ID, "result": result, "error": rpcErr}
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(resp)
}

func newTestClient(server *httptest.Server) *BitcoinRPCClient {
	u, _ := url.Parse(server.URL)
	return NewBitcoinRPCClient(&BitcoinRPCConfig{
		Host:     u.Host,
		User:     "testuser",
		Password: "testpass",
	})
}

func fullMockRPC() *mockRPC {
	m := newMockRPC()
	m.handlers["createrawtransaction"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return "rawhex000", nil
	}
	m.handlers["fundrawtransaction"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return map[string]any{"hex": "fundedhex000", "fee": 0.00001}, nil
	}
	m.handlers["signrawtransactionwithwallet"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return map[string]any{"hex": "signedhex000", "complete": true}, nil
	}
	m.handlers["sendrawtransaction"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return "abc123txid", nil
	}
	return m
}

// ---------------------------------------------------------------------------
// call() - low-level RPC
// ---------------------------------------------------------------------------

func TestCall_BasicAuth(t *testing.T) {
	m := newMockRPC()
	m.handlers["test"] = func(_ json.RawMessage) (any, *mockRPCErr) { return "ok", nil }
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	client.call("test", []any{})
	if !m.lastAuthOK {
		t.Error("expected basic auth to be set")
	}
}

func TestCall_URLWithoutWallet(t *testing.T) {
	m := newMockRPC()
	m.handlers["test"] = func(_ json.RawMessage) (any, *mockRPCErr) { return "ok", nil }
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	client.call("test", []any{})
	if m.lastPath != "/" {
		t.Errorf("expected path /, got %s", m.lastPath)
	}
}

func TestCall_URLWithWallet(t *testing.T) {
	m := newMockRPC()
	m.handlers["test"] = func(_ json.RawMessage) (any, *mockRPCErr) { return "ok", nil }
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv).WithWallet("mywallet")

	client.call("test", []any{})
	if m.lastPath != "/wallet/mywallet" {
		t.Errorf("expected /wallet/mywallet, got %s", m.lastPath)
	}
}

func TestCall_RPCError(t *testing.T) {
	m := newMockRPC()
	m.handlers["fail"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -4, Message: "wallet not found"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.call("fail", []any{})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "wallet not found") {
		t.Errorf("expected wallet not found in error, got: %v", err)
	}
}

func TestCall_HTTP401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.call("test", []any{})
	if err == nil || !strings.Contains(err.Error(), "authentication failed (401)") {
		t.Errorf("expected 401 auth error, got: %v", err)
	}
}

func TestCall_HTTP403(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.call("test", []any{})
	if err == nil || !strings.Contains(err.Error(), "forbidden (403)") {
		t.Errorf("expected 403 error, got: %v", err)
	}
}

func TestCall_HTTP500(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("internal error"))
	}))
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.call("test", []any{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("expected HTTP 500 error, got: %v", err)
	}
}

func TestCall_InvalidJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("not json"))
	}))
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.call("test", []any{})
	if err == nil || !strings.Contains(err.Error(), "failed to unmarshal response") {
		t.Errorf("expected unmarshal error, got: %v", err)
	}
}

func TestCall_ServerDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	client := newTestClient(srv)

	_, err := client.call("test", []any{})
	if err == nil || !strings.Contains(err.Error(), "failed to send request") {
		t.Errorf("expected connection error, got: %v", err)
	}
}

func TestCall_MethodNotFound(t *testing.T) {
	m := newMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.call("nonexistent", []any{})
	if err == nil || !strings.Contains(err.Error(), "Method not found") {
		t.Errorf("expected method not found, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// WithWallet
// ---------------------------------------------------------------------------

func TestWithWallet(t *testing.T) {
	client := NewBitcoinRPCClient(&BitcoinRPCConfig{Host: "localhost:1234"})
	result := client.WithWallet("testwallet")
	if result != client {
		t.Error("WithWallet should return same client")
	}
	if client.wallet != "testwallet" {
		t.Errorf("expected wallet=testwallet, got %s", client.wallet)
	}
}

// ---------------------------------------------------------------------------
// GetBlockCount
// ---------------------------------------------------------------------------

func TestGetBlockCount(t *testing.T) {
	m := newMockRPC()
	m.handlers["getblockcount"] = func(_ json.RawMessage) (any, *mockRPCErr) { return 12345, nil }
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	count, err := client.GetBlockCount()
	if err != nil {
		t.Fatal(err)
	}
	if count != 12345 {
		t.Errorf("expected 12345, got %d", count)
	}
}

func TestGetBlockCount_Error(t *testing.T) {
	m := newMockRPC()
	m.handlers["getblockcount"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -1, Message: "loading"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.GetBlockCount()
	if err == nil {
		t.Error("expected error")
	}
}

// ---------------------------------------------------------------------------
// GetBlockchainInfo
// ---------------------------------------------------------------------------

func TestGetBlockchainInfo(t *testing.T) {
	m := newMockRPC()
	m.handlers["getblockchaininfo"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return map[string]any{
			"chain":                "signet",
			"blocks":               200000,
			"headers":              200000,
			"bestblockhash":        "0000abc",
			"difficulty":           0.001,
			"verificationprogress": 1.0,
			"pruned":               false,
		}, nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	info, err := client.GetBlockchainInfo()
	if err != nil {
		t.Fatal(err)
	}
	if info.Chain != "signet" {
		t.Errorf("expected signet, got %s", info.Chain)
	}
	if info.Blocks != 200000 {
		t.Errorf("expected 200000 blocks, got %d", info.Blocks)
	}
	if info.Pruned {
		t.Error("expected pruned=false")
	}
}

func TestGetBlockchainInfo_Error(t *testing.T) {
	m := newMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.GetBlockchainInfo()
	if err == nil {
		t.Error("expected error for unregistered method")
	}
}

// ---------------------------------------------------------------------------
// ListWallets
// ---------------------------------------------------------------------------

func TestListWallets(t *testing.T) {
	m := newMockRPC()
	m.handlers["listwallets"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return []string{"faucet", "default"}, nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	wallets, err := client.ListWallets()
	if err != nil {
		t.Fatal(err)
	}
	if len(wallets) != 2 {
		t.Fatalf("expected 2 wallets, got %d", len(wallets))
	}
	if wallets[0] != "faucet" || wallets[1] != "default" {
		t.Errorf("unexpected wallets: %v", wallets)
	}
}

// ---------------------------------------------------------------------------
// LoadWallet
// ---------------------------------------------------------------------------

func TestLoadWallet(t *testing.T) {
	m := newMockRPC()
	m.handlers["loadwallet"] = func(params json.RawMessage) (any, *mockRPCErr) {
		return map[string]any{"name": "faucet"}, nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	err := client.LoadWallet("faucet")
	if err != nil {
		t.Fatal(err)
	}

	var p []any
	json.Unmarshal(m.lastParams, &p)
	if p[0].(string) != "faucet" {
		t.Errorf("expected wallet name param=faucet, got %v", p[0])
	}
}

func TestLoadWallet_Error(t *testing.T) {
	m := newMockRPC()
	m.handlers["loadwallet"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -18, Message: "Wallet file not found"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	err := client.LoadWallet("missing")
	if err == nil || !strings.Contains(err.Error(), "Wallet file not found") {
		t.Errorf("expected wallet not found error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetBalances
// ---------------------------------------------------------------------------

func TestGetBalances(t *testing.T) {
	m := newMockRPC()
	m.handlers["getbalances"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return map[string]any{
			"mine": map[string]any{
				"trusted":           10.5,
				"untrusted_pending": 1.0,
				"immature":          0.25,
			},
		}, nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	bal, err := client.GetBalances()
	if err != nil {
		t.Fatal(err)
	}
	if bal.Mine.Trusted != 10.5 {
		t.Errorf("expected trusted=10.5, got %f", bal.Mine.Trusted)
	}
	if bal.Mine.Untrusted != 1.0 {
		t.Errorf("expected untrusted=1.0, got %f", bal.Mine.Untrusted)
	}
	if bal.Mine.Immature != 0.25 {
		t.Errorf("expected immature=0.25, got %f", bal.Mine.Immature)
	}
}

// ---------------------------------------------------------------------------
// GetNewAddress
// ---------------------------------------------------------------------------

func TestGetNewAddress(t *testing.T) {
	m := newMockRPC()
	m.handlers["getnewaddress"] = func(params json.RawMessage) (any, *mockRPCErr) {
		return "tb1qnewaddr", nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	addr, err := client.GetNewAddress("mylabel", "bech32")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "tb1qnewaddr" {
		t.Errorf("expected tb1qnewaddr, got %s", addr)
	}

	var p []any
	json.Unmarshal(m.lastParams, &p)
	if len(p) != 2 || p[0].(string) != "mylabel" || p[1].(string) != "bech32" {
		t.Errorf("unexpected params: %v", p)
	}
}

func TestGetNewAddress_NoParams(t *testing.T) {
	m := newMockRPC()
	m.handlers["getnewaddress"] = func(params json.RawMessage) (any, *mockRPCErr) {
		return "tb1qdefault", nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	addr, err := client.GetNewAddress("", "")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "tb1qdefault" {
		t.Errorf("expected tb1qdefault, got %s", addr)
	}

	var p []any
	json.Unmarshal(m.lastParams, &p)
	if len(p) != 0 {
		t.Errorf("expected empty params, got %v", p)
	}
}

func TestGetNewAddress_LabelOnly(t *testing.T) {
	m := newMockRPC()
	m.handlers["getnewaddress"] = func(_ json.RawMessage) (any, *mockRPCErr) { return "tb1q", nil }
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	client.GetNewAddress("label", "")
	var p []any
	json.Unmarshal(m.lastParams, &p)
	if len(p) != 1 || p[0].(string) != "label" {
		t.Errorf("expected [label], got %v", p)
	}
}

// ---------------------------------------------------------------------------
// ListUnspent
// ---------------------------------------------------------------------------

func TestListUnspent(t *testing.T) {
	m := newMockRPC()
	m.handlers["listunspent"] = func(params json.RawMessage) (any, *mockRPCErr) {
		return []map[string]any{
			{"txid": "aaa", "vout": 0, "address": "tb1q1", "amount": 0.5, "confirmations": 10, "spendable": true},
			{"txid": "bbb", "vout": 1, "address": "tb1q2", "amount": 1.0, "confirmations": 0, "spendable": false},
		}, nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	utxos, err := client.ListUnspent(0, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if len(utxos) != 2 {
		t.Fatalf("expected 2 utxos, got %d", len(utxos))
	}
	if utxos[0].TxID != "aaa" || utxos[0].Amount != 0.5 || !utxos[0].Spendable {
		t.Errorf("unexpected utxo[0]: %+v", utxos[0])
	}
	if utxos[1].Spendable {
		t.Error("expected utxo[1] not spendable")
	}

	var p []any
	json.Unmarshal(m.lastParams, &p)
	if p[0].(float64) != 0 || p[1].(float64) != 9999 {
		t.Errorf("unexpected params: %v", p)
	}
}

func TestListUnspent_Empty(t *testing.T) {
	m := newMockRPC()
	m.handlers["listunspent"] = func(_ json.RawMessage) (any, *mockRPCErr) { return []any{}, nil }
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	utxos, err := client.ListUnspent(1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(utxos) != 0 {
		t.Errorf("expected 0 utxos, got %d", len(utxos))
	}
}

// ---------------------------------------------------------------------------
// SendToAddressWithOpReturn
// ---------------------------------------------------------------------------

func TestSendToAddress_Success(t *testing.T) {
	m := fullMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	txid, err := client.SendToAddressWithOpReturn("tb1qaddr", 0.05, 1.0, "hello")
	if err != nil {
		t.Fatal(err)
	}
	if txid != "abc123txid" {
		t.Errorf("expected abc123txid, got %s", txid)
	}
	if m.methodCalls["createrawtransaction"] != 1 {
		t.Error("expected createrawtransaction to be called")
	}
	if m.methodCalls["fundrawtransaction"] != 1 {
		t.Error("expected fundrawtransaction to be called")
	}
	if m.methodCalls["signrawtransactionwithwallet"] != 1 {
		t.Error("expected signrawtransactionwithwallet to be called")
	}
	if m.methodCalls["sendrawtransaction"] != 1 {
		t.Error("expected sendrawtransaction to be called")
	}
}

func TestSendToAddress_NoOpReturn(t *testing.T) {
	m := fullMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	txid, err := client.SendToAddressWithOpReturn("tb1qaddr", 0.05, 1.0, "")
	if err != nil {
		t.Fatal(err)
	}
	if txid != "abc123txid" {
		t.Errorf("expected abc123txid, got %s", txid)
	}
}

func TestSendToAddress_NoFeeRate(t *testing.T) {
	m := fullMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	txid, err := client.SendToAddressWithOpReturn("tb1qaddr", 0.05, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if txid != "abc123txid" {
		t.Errorf("expected abc123txid, got %s", txid)
	}
}

func TestSendToAddress_DustAmount(t *testing.T) {
	m := fullMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.SendToAddressWithOpReturn("tb1qaddr", 0.000001, 1.0, "")
	if err == nil || !strings.Contains(err.Error(), "Amount too low") {
		t.Errorf("expected dust error, got: %v", err)
	}
}

func TestSendToAddress_CreateRawFails(t *testing.T) {
	m := fullMockRPC()
	m.handlers["createrawtransaction"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -1, Message: "bad params"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.SendToAddressWithOpReturn("tb1q", 0.05, 1.0, "")
	if err == nil || !strings.Contains(err.Error(), "createrawtransaction failed") {
		t.Errorf("expected createrawtransaction error, got: %v", err)
	}
}

func TestSendToAddress_FundRawFails(t *testing.T) {
	m := fullMockRPC()
	m.handlers["fundrawtransaction"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -6, Message: "Insufficient funds"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.SendToAddressWithOpReturn("tb1q", 0.05, 1.0, "")
	if err == nil || !strings.Contains(err.Error(), "fundrawtransaction failed") {
		t.Errorf("expected fundrawtransaction error, got: %v", err)
	}
}

func TestSendToAddress_SignFails(t *testing.T) {
	m := fullMockRPC()
	m.handlers["signrawtransactionwithwallet"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -1, Message: "signing error"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.SendToAddressWithOpReturn("tb1q", 0.05, 1.0, "")
	if err == nil || !strings.Contains(err.Error(), "signrawtransactionwithwallet failed") {
		t.Errorf("expected sign error, got: %v", err)
	}
}

func TestSendToAddress_SignIncomplete(t *testing.T) {
	m := fullMockRPC()
	m.handlers["signrawtransactionwithwallet"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return map[string]any{"hex": "partial", "complete": false}, nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.SendToAddressWithOpReturn("tb1q", 0.05, 1.0, "")
	if err == nil || !strings.Contains(err.Error(), "signing incomplete") {
		t.Errorf("expected signing incomplete, got: %v", err)
	}
}

func TestSendToAddress_SendRawFails(t *testing.T) {
	m := fullMockRPC()
	m.handlers["sendrawtransaction"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -25, Message: "missing inputs"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.SendToAddressWithOpReturn("tb1q", 0.05, 1.0, "")
	if err == nil || !strings.Contains(err.Error(), "sendrawtransaction failed") {
		t.Errorf("expected sendrawtransaction error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Consolidate
// ---------------------------------------------------------------------------

func TestConsolidate_Success(t *testing.T) {
	m := fullMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	utxos := []UTXO{
		{TxID: "tx1", Vout: 0, Amount: 0.001},
		{TxID: "tx2", Vout: 1, Amount: 0.002},
	}

	txid, err := client.Consolidate(utxos, 0.003, "tb1qconsolidated", "faucet")
	if err != nil {
		t.Fatal(err)
	}
	if txid != "abc123txid" {
		t.Errorf("expected abc123txid, got %s", txid)
	}

	if m.methodCalls["createrawtransaction"] != 1 {
		t.Error("expected createrawtransaction")
	}
	if m.methodCalls["signrawtransactionwithwallet"] != 1 {
		t.Error("expected signrawtransactionwithwallet")
	}
	if m.methodCalls["sendrawtransaction"] != 1 {
		t.Error("expected sendrawtransaction")
	}
	if m.methodCalls["fundrawtransaction"] != 0 {
		t.Error("consolidate should NOT call fundrawtransaction")
	}
}

func TestConsolidate_NoOpReturn(t *testing.T) {
	m := fullMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	utxos := []UTXO{{TxID: "tx1", Vout: 0, Amount: 0.001}}
	_, err := client.Consolidate(utxos, 0.001, "tb1q", "")
	if err != nil {
		t.Fatal(err)
	}
}

func TestConsolidate_AmountTooSmall(t *testing.T) {
	m := fullMockRPC()
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	utxos := []UTXO{{TxID: "tx1", Vout: 0, Amount: 0.00000001}}
	_, err := client.Consolidate(utxos, 0.00000001, "tb1q", "faucet")
	if err == nil || !strings.Contains(err.Error(), "too small to cover fees") {
		t.Errorf("expected fee error, got: %v", err)
	}
}

func TestConsolidate_SortsInputsByAmount(t *testing.T) {
	m := fullMockRPC()
	var capturedParams json.RawMessage
	m.handlers["createrawtransaction"] = func(params json.RawMessage) (any, *mockRPCErr) {
		capturedParams = params
		return "rawhex", nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	utxos := []UTXO{
		{TxID: "small", Vout: 0, Amount: 0.001},
		{TxID: "big", Vout: 0, Amount: 0.005},
		{TxID: "med", Vout: 0, Amount: 0.003},
	}

	client.Consolidate(utxos, 0.009, "tb1q", "")

	var p []json.RawMessage
	json.Unmarshal(capturedParams, &p)
	var inputs []map[string]any
	json.Unmarshal(p[0], &inputs)

	if inputs[0]["txid"].(string) != "big" {
		t.Errorf("expected inputs sorted desc by amount, first=%s", inputs[0]["txid"])
	}
	if inputs[2]["txid"].(string) != "small" {
		t.Errorf("expected last=small, got %s", inputs[2]["txid"])
	}
}

func TestConsolidate_CreateRawFails(t *testing.T) {
	m := fullMockRPC()
	m.handlers["createrawtransaction"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -1, Message: "error"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.Consolidate([]UTXO{{TxID: "t", Amount: 0.01}}, 0.01, "tb1q", "")
	if err == nil || !strings.Contains(err.Error(), "createrawtransaction failed") {
		t.Errorf("expected error, got: %v", err)
	}
}

func TestConsolidate_SignIncomplete(t *testing.T) {
	m := fullMockRPC()
	m.handlers["signrawtransactionwithwallet"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return map[string]any{"hex": "x", "complete": false}, nil
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.Consolidate([]UTXO{{TxID: "t", Amount: 0.01}}, 0.01, "tb1q", "")
	if err == nil || !strings.Contains(err.Error(), "signing incomplete") {
		t.Errorf("expected signing incomplete, got: %v", err)
	}
}

func TestConsolidate_SendFails(t *testing.T) {
	m := fullMockRPC()
	m.handlers["sendrawtransaction"] = func(_ json.RawMessage) (any, *mockRPCErr) {
		return nil, &mockRPCErr{Code: -25, Message: "bad-txns"}
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	client := newTestClient(srv)

	_, err := client.Consolidate([]UTXO{{TxID: "t", Amount: 0.01}}, 0.01, "tb1q", "")
	if err == nil || !strings.Contains(err.Error(), "sendrawtransaction failed") {
		t.Errorf("expected error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// ValidateSignetAddress
// ---------------------------------------------------------------------------

func TestValidateSignetAddress(t *testing.T) {
	tests := []struct {
		addr    string
		wantErr bool
		desc    string
	}{
		// valid bech32 (signet)
		{"tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx", false, "valid bech32"},
		{"tb1qrp33g0q5b5698ahp5jnf012tlhpe0efxqrnz0la4el8v0k8svs5s3anvl5", false, "valid bech32m taproot-length"},

		// valid P2SH
		{"2N1rjhumXA3ephUQTDMfGhufxGaN1Lap4Ji", false, "valid P2SH"},

		// valid P2PKH
		{"mipcBbFg9gMiCh81Kj8tqqdgoZub1ZJRfn", false, "valid P2PKH m-prefix"},
		{"n3GNqMveyvaPvUbH469vDRadqpJMPc84JA", false, "valid P2PKH n-prefix"},

		// mainnet addresses rejected
		{"bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", true, "mainnet bech32"},
		{"1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", true, "mainnet P2PKH"},
		{"3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy", true, "mainnet P2SH"},

		// invalid
		{"", true, "empty"},
		{"  ", true, "whitespace"},
		{"not_an_address", true, "garbage"},
		{"tb1short", true, "too short bech32"},
		{"TB1QW508D6QEJXTDG4Y5R3ZARVARY0C5XW7KXPJZSX", true, "uppercase bech32"},

		// whitespace trimming
		{"  tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx  ", false, "trimmed bech32"},
	}

	for _, tt := range tests {
		err := ValidateSignetAddress(tt.addr)
		if (err != nil) != tt.wantErr {
			t.Errorf("%s: ValidateSignetAddress(%q) error=%v, wantErr=%v", tt.desc, tt.addr, err, tt.wantErr)
		}
	}
}

func TestValidateSignetAddress_MainnetError(t *testing.T) {
	err := ValidateSignetAddress("bc1qtest")
	if err == nil || !strings.Contains(err.Error(), "mainnet") {
		t.Errorf("expected mainnet error, got: %v", err)
	}
}

func TestValidateSignetAddress_EmptyError(t *testing.T) {
	err := ValidateSignetAddress("")
	if err == nil || !strings.Contains(err.Error(), "cannot be empty") {
		t.Errorf("expected empty error, got: %v", err)
	}
}
