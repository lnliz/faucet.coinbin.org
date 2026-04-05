package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lnliz/faucet.coinbin.org/btc"
	"github.com/lnliz/faucet.coinbin.org/db"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// ---------------------------------------------------------------------------
// mock bitcoin RPC server
// ---------------------------------------------------------------------------

type rpcRequest struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     string          `json:"id"`
}

type mockRPC struct {
	handlers map[string]func(params json.RawMessage) (any, *rpcErr)
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func newMockRPC() *mockRPC {
	m := &mockRPC{handlers: make(map[string]func(json.RawMessage) (any, *rpcErr))}

	m.handlers["getblockchaininfo"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{"chain": "signet", "blocks": 100}, nil
	}
	m.handlers["listwallets"] = func(_ json.RawMessage) (any, *rpcErr) {
		return []string{"faucet"}, nil
	}
	m.handlers["loadwallet"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{"name": "faucet"}, nil
	}
	m.handlers["getbalances"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{
			"mine": map[string]any{
				"trusted":           10.0,
				"untrusted_pending": 1.0,
				"immature":          0.5,
			},
		}, nil
	}
	m.handlers["getnewaddress"] = func(_ json.RawMessage) (any, *rpcErr) {
		return "tb1qnewaddress000000000000000000000000000", nil
	}
	m.handlers["listunspent"] = func(_ json.RawMessage) (any, *rpcErr) {
		return []btc.UTXO{
			{TxID: "aaa", Vout: 0, Address: "tb1qaddr1", Amount: 0.0005, Confirmations: 10, Spendable: true},
			{TxID: "bbb", Vout: 1, Address: "tb1qaddr2", Amount: 0.0003, Confirmations: 5, Spendable: true},
			{TxID: "ccc", Vout: 0, Address: "tb1qaddr3", Amount: 1.5, Confirmations: 100, Spendable: true},
		}, nil
	}
	m.handlers["createrawtransaction"] = func(_ json.RawMessage) (any, *rpcErr) {
		return "rawhex000", nil
	}
	m.handlers["fundrawtransaction"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{"hex": "fundedhex000", "fee": 0.00001}, nil
	}
	m.handlers["signrawtransactionwithwallet"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{"hex": "signedhex000", "complete": true}, nil
	}
	m.handlers["sendrawtransaction"] = func(_ json.RawMessage) (any, *rpcErr) {
		return "mocktxid0000000000000000000000000000000000000000000000000000000000", nil
	}

	return m
}

func (m *mockRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req rpcRequest
	json.Unmarshal(body, &req)

	handler, ok := m.handlers[req.Method]
	if !ok {
		w.WriteHeader(200)
		json.NewEncoder(w).Encode(map[string]any{
			"result": nil,
			"error":  &rpcErr{Code: -32601, Message: "Method not found: " + req.Method},
			"id":     req.ID,
		})
		return
	}

	result, rpcError := handler(req.Params)
	resp := map[string]any{"id": req.ID, "result": result, "error": rpcError}
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// test helpers
// ---------------------------------------------------------------------------

func testDB(t *testing.T) *gorm.DB {
	t.Helper()
	d, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	d.AutoMigrate(&db.Transaction{}, &db.AdminSession{})
	return d
}

func parseCIDR(s string) net.IPNet {
	_, n, _ := net.ParseCIDR(s)
	return *n
}

func testConfig() *Config {
	return &Config{
		ListenAddr:                      ":0",
		MetricsAddr:                     "127.0.0.1:0",
		DataDir:                         "/tmp/test",
		BitcoinCoreWalletName:           "faucet",
		BatchInterval:                   time.Minute,
		MinBalance:                      0.1,
		AdminPassword:                   "testpass123",
		AdminPath:                       "/admin",
		AdminCookieSecret:               "01234567890123456789012345678901",
		AdminAllowlist:                  []net.IPNet{parseCIDR("127.0.0.1/32")},
		MaxWithdrawalsPerIP24h:          2,
		MaxDepositsPerAddress:           5,
		EnabledAmountRanges:             []int{1, 2, 3},
		DefaultAmountRange:              2,
		ConsolidationAmountThresholdBTC: 0.001,
		MaxConsolidationUTXOs:           5,
		MinConsolidationUTXOs:           2,
	}
}

func testService(t *testing.T, rpcServer *httptest.Server) *Service {
	t.Helper()
	cfg := testConfig()
	u, _ := url.Parse(rpcServer.URL)
	cfg.BitcoinRPC = btc.BitcoinRPCConfig{Host: u.Host, User: "user", Password: "pass"}

	database := testDB(t)
	return NewService(cfg, database)
}

func testServiceFull(t *testing.T) (*Service, *httptest.Server) {
	t.Helper()
	mock := newMockRPC()
	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)
	return svc, rpcServer
}

func adminLogin(t *testing.T, svc *Service) string {
	t.Helper()
	sessionID := "test-session-id"
	svc.db.Create(&db.AdminSession{
		SessionID: sessionID,
		IPAddress: "127.0.0.1",
		ExpiresAt: time.Now().Add(4 * time.Hour),
	})
	return svc.signCookie(sessionID)
}

func jsonBody(v any) *bytes.Buffer {
	b, _ := json.Marshal(v)
	return bytes.NewBuffer(b)
}

func decodeJSON(t *testing.T, body io.Reader) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.NewDecoder(body).Decode(&m); err != nil {
		t.Fatalf("failed to decode json: %v", err)
	}
	return m
}

// startTestServer creates the full HTTP server stack (with middleware) and returns its URL
func startTestServer(t *testing.T, svc *Service) string {
	t.Helper()
	chdirToProjectRoot(t)
	server := svc.StartService()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	return fmt.Sprintf("http://%s", listener.Addr().String())
}

func chdirToProjectRoot(t *testing.T) {
	t.Helper()
	// templates are at project root, tests run from service/
	if err := os.Chdir(".."); err != nil {
		t.Fatalf("failed to chdir to project root: %v", err)
	}
	t.Cleanup(func() { os.Chdir("service") })
}

// ---------------------------------------------------------------------------
// getClientIP tests
// ---------------------------------------------------------------------------

func TestGetClientIP_CFConnectingIP(t *testing.T) {
	svc, _ := testServiceFull(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("CF-Connecting-IP", "1.2.3.4")
	if got := svc.getClientIP(r); got != "1.2.3.4" {
		t.Errorf("expected 1.2.3.4, got %s", got)
	}
}

func TestGetClientIP_XForwardedFor(t *testing.T) {
	svc, _ := testServiceFull(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "5.6.7.8, 9.10.11.12")
	if got := svc.getClientIP(r); got != "5.6.7.8" {
		t.Errorf("expected 5.6.7.8, got %s", got)
	}
}

func TestGetClientIP_XRealIP(t *testing.T) {
	svc, _ := testServiceFull(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Real-IP", "10.0.0.1")
	if got := svc.getClientIP(r); got != "10.0.0.1" {
		t.Errorf("expected 10.0.0.1, got %s", got)
	}
}

func TestGetClientIP_RemoteAddr(t *testing.T) {
	svc, _ := testServiceFull(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "192.168.1.1:12345"
	if got := svc.getClientIP(r); got != "192.168.1.1" {
		t.Errorf("expected 192.168.1.1, got %s", got)
	}
}

func TestGetClientIP_Priority(t *testing.T) {
	svc, _ := testServiceFull(t)
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("CF-Connecting-IP", "1.1.1.1")
	r.Header.Set("X-Forwarded-For", "2.2.2.2")
	r.Header.Set("X-Real-IP", "3.3.3.3")
	r.RemoteAddr = "4.4.4.4:1234"
	if got := svc.getClientIP(r); got != "1.1.1.1" {
		t.Errorf("CF-Connecting-IP should take priority, got %s", got)
	}
}

// ---------------------------------------------------------------------------
// isAdminIP tests
// ---------------------------------------------------------------------------

func TestIsAdminIP(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{
		parseCIDR("127.0.0.1/32"),
		parseCIDR("10.0.0.0/8"),
	}

	tests := []struct {
		ip   string
		want bool
	}{
		{"127.0.0.1", true},
		{"10.0.0.1", true},
		{"10.255.255.255", true},
		{"11.0.0.1", false},
		{"192.168.1.1", false},
		{"invalid", false},
		{"", false},
	}

	for _, tt := range tests {
		if got := svc.isAdminIP(tt.ip); got != tt.want {
			t.Errorf("isAdminIP(%q) = %v, want %v", tt.ip, got, tt.want)
		}
	}
}

func TestIsAdminIP_AllowAll(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("0.0.0.0/0")}

	for _, ip := range []string{"1.2.3.4", "10.0.0.1", "192.168.1.1", "255.255.255.255"} {
		if !svc.isAdminIP(ip) {
			t.Errorf("0.0.0.0/0 should allow %s", ip)
		}
	}
}

// ---------------------------------------------------------------------------
// cookie signing / validation
// ---------------------------------------------------------------------------

func TestSignAndValidateCookie(t *testing.T) {
	svc, _ := testServiceFull(t)

	signed := svc.signCookie("session123")
	id, valid := svc.validateSessionCookie(signed)
	if !valid || id != "session123" {
		t.Errorf("expected valid cookie with id session123, got valid=%v id=%s", valid, id)
	}
}

func TestValidateSessionCookie_Invalid(t *testing.T) {
	svc, _ := testServiceFull(t)

	tests := []string{
		"noseparator",
		"bad.signature",
		"a.b.c",
		"",
	}

	for _, c := range tests {
		_, valid := svc.validateSessionCookie(c)
		if valid {
			t.Errorf("expected invalid for %q", c)
		}
	}
}

func TestValidateSessionCookie_WrongSecret(t *testing.T) {
	svc, _ := testServiceFull(t)
	signed := svc.signCookie("session123")

	svc.cfg.AdminCookieSecret = "different_secret_01234567890123456789"
	_, valid := svc.validateSessionCookie(signed)
	if valid {
		t.Error("cookie signed with old secret should not validate")
	}
}

// ---------------------------------------------------------------------------
// GetEnabledAmountRanges / GetAmountRangeByID
// ---------------------------------------------------------------------------

func TestGetEnabledAmountRanges(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.EnabledAmountRanges = []int{1, 3}

	ranges := svc.GetEnabledAmountRanges()
	if len(ranges) != 2 {
		t.Fatalf("expected 2 ranges, got %d", len(ranges))
	}
	if ranges[0].ID != 1 || ranges[1].ID != 3 {
		t.Errorf("expected IDs [1,3], got [%d,%d]", ranges[0].ID, ranges[1].ID)
	}
}

func TestGetAmountRangeByID(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.EnabledAmountRanges = []int{2, 3}

	if r := svc.GetAmountRangeByID(2); r == nil || r.ID != 2 {
		t.Error("expected range 2")
	}
	if r := svc.GetAmountRangeByID(1); r != nil {
		t.Error("range 1 not enabled, should be nil")
	}
	if r := svc.GetAmountRangeByID(99); r != nil {
		t.Error("range 99 doesn't exist, should be nil")
	}
}

// ---------------------------------------------------------------------------
// GetCachedWalletBalance
// ---------------------------------------------------------------------------

func TestGetCachedWalletBalance(t *testing.T) {
	svc, _ := testServiceFull(t)

	if svc.GetCachedWalletBalance() != 0 {
		t.Error("expected 0 before refresh")
	}

	svc.walletBalanceMtx.Lock()
	svc.walletBalance = 5.5
	svc.walletBalanceMtx.Unlock()

	if got := svc.GetCachedWalletBalance(); got != 5.5 {
		t.Errorf("expected 5.5, got %f", got)
	}
}

// ---------------------------------------------------------------------------
// health endpoint
// ---------------------------------------------------------------------------

func TestHealthHandler_OK(t *testing.T) {
	svc, _ := testServiceFull(t)
	r := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	svc.healthHandler(w, r)

	if w.Code != 200 {
		t.Errorf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "ok" {
		t.Errorf("expected 'ok', got %q", w.Body.String())
	}
}

func TestHealthHandler_RPCDown(t *testing.T) {
	mock := newMockRPC()
	mock.handlers["getblockchaininfo"] = func(_ json.RawMessage) (any, *rpcErr) {
		return nil, &rpcErr{Code: -1, Message: "connection refused"}
	}
	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)

	r := httptest.NewRequest("GET", "/health", nil)
	w := httptest.NewRecorder()
	svc.healthHandler(w, r)

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// submit endpoint
// ---------------------------------------------------------------------------

func TestSubmitHandler_MethodNotAllowed(t *testing.T) {
	svc, _ := testServiceFull(t)

	r := httptest.NewRequest("GET", "/api/submit", nil)
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestSubmitHandler_InvalidJSON(t *testing.T) {
	svc, _ := testServiceFull(t)

	r := httptest.NewRequest("POST", "/api/submit", strings.NewReader("not json"))
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSubmitHandler_InvalidAddress(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{"address": "invalid", "amount_range": 2})
	r := httptest.NewRequest("POST", "/api/submit", body)
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	resp := decodeJSON(t, w.Body)
	if resp["error"] == nil {
		t.Error("expected error in response")
	}
}

func TestSubmitHandler_MainnetAddressRejected(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{"address": "bc1qw508d6qejxtdg4y5r3zarvary0c5xw7kv8f3t4", "amount_range": 2})
	r := httptest.NewRequest("POST", "/api/submit", body)
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestSubmitHandler_Success(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{
		"address":      "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount_range": 2,
	})
	r := httptest.NewRequest("POST", "/api/submit", body)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	resp := decodeJSON(t, w.Body)
	if resp["success"] != true {
		t.Errorf("expected success=true, got %v", resp["success"])
	}

	var tx db.Transaction
	svc.db.First(&tx)
	if tx.Address != "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx" {
		t.Errorf("unexpected address in db: %s", tx.Address)
	}
	if tx.Status != db.TxnStatusPending {
		t.Errorf("expected pending status, got %s", tx.Status)
	}
	if tx.AmountBTC < 0.01 || tx.AmountBTC > 0.09 {
		t.Errorf("amount %.8f out of range 2 bounds", tx.AmountBTC)
	}
}

func TestSubmitHandler_DefaultAmountRange(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{
		"address":      "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount_range": 999,
	})
	r := httptest.NewRequest("POST", "/api/submit", body)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200 (fallback to default range), got %d: %s", w.Code, w.Body.String())
	}

	var tx db.Transaction
	svc.db.Last(&tx)
	if tx.AmountBTC < 0.01 || tx.AmountBTC > 0.09 {
		t.Errorf("amount %.8f should be in default range 2", tx.AmountBTC)
	}
}

func TestSubmitHandler_RateLimitNonAdmin(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.MaxWithdrawalsPerIP24h = 1
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("10.0.0.0/8")}

	body := jsonBody(map[string]any{
		"address":      "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount_range": 2,
	})

	r := httptest.NewRequest("POST", "/api/submit", body)
	r.RemoteAddr = "192.168.1.1:1234"
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("first request should succeed, got %d", w.Code)
	}

	body2 := jsonBody(map[string]any{
		"address":      "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount_range": 2,
	})
	r2 := httptest.NewRequest("POST", "/api/submit", body2)
	r2.RemoteAddr = "192.168.1.1:1234"
	w2 := httptest.NewRecorder()
	svc.submitHandler(w2, r2)

	if w2.Code != http.StatusTooManyRequests {
		t.Errorf("expected 429 rate limit, got %d", w2.Code)
	}
}

func TestSubmitHandler_AdminBypassesRateLimit(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.MaxWithdrawalsPerIP24h = 1
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}

	for i := range 3 {
		body := jsonBody(map[string]any{
			"address":      "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
			"amount_range": 2,
		})
		r := httptest.NewRequest("POST", "/api/submit", body)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		svc.submitHandler(w, r)

		if w.Code != http.StatusOK {
			t.Errorf("admin request %d should succeed, got %d: %s", i, w.Code, w.Body.String())
		}
	}
}

func TestSubmitHandler_AddressDepositLimit(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.MaxDepositsPerAddress = 2
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}

	addr := "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx"

	for i := range 2 {
		body := jsonBody(map[string]any{"address": addr, "amount_range": 2})
		r := httptest.NewRequest("POST", "/api/submit", body)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		svc.submitHandler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d should succeed, got %d", i, w.Code)
		}
	}

	body := jsonBody(map[string]any{"address": addr, "amount_range": 2})
	r := httptest.NewRequest("POST", "/api/submit", body)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400 address limit, got %d", w.Code)
	}
	resp := decodeJSON(t, w.Body)
	if !strings.Contains(resp["error"].(string), "limit") {
		t.Errorf("expected limit error, got: %s", resp["error"])
	}
}

// ---------------------------------------------------------------------------
// admin IP allowlist middleware (integration via full server)
// ---------------------------------------------------------------------------

func TestAdminIPAllowlist_Denied(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("10.0.0.0/8")}
	baseURL := startTestServer(t, svc)

	resp, err := http.Get(baseURL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 from non-allowed IP, got %d", resp.StatusCode)
	}
}

func TestAdminIPAllowlist_Allowed(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	resp, err := http.Get(baseURL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from allowed IP, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// admin auth middleware
// ---------------------------------------------------------------------------

func TestAdminAuth_NoCookie(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.Get(baseURL + "/admin/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302 redirect to login, got %d", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if !strings.Contains(loc, "/admin/login") {
		t.Errorf("expected redirect to /admin/login, got %s", loc)
	}
}

func TestAdminAuth_InvalidCookie(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	req, _ := http.NewRequest("GET", baseURL+"/admin/", nil)
	req.AddCookie(&http.Cookie{Name: "admin_session", Value: "bad.cookie"})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302 redirect, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// admin login flow
// ---------------------------------------------------------------------------

func TestAdminLogin_GetPage(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	resp, err := http.Get(baseURL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

func TestAdminLogin_WrongPassword(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	resp, err := http.PostForm(baseURL+"/admin/login", url.Values{
		"password": {"wrongpassword"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", resp.StatusCode)
	}
}

func TestAdminLogin_Success(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	resp, err := client.PostForm(baseURL+"/admin/login", url.Values{
		"password": {"testpass123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302 redirect after login, got %d", resp.StatusCode)
	}

	cookies := resp.Cookies()
	found := false
	for _, c := range cookies {
		if c.Name == "admin_session" && c.Value != "" {
			found = true
			_, valid := svc.validateSessionCookie(c.Value)
			if !valid {
				t.Error("session cookie is not valid")
			}
		}
	}
	if !found {
		t.Error("expected admin_session cookie to be set")
	}

	var count int64
	svc.db.Model(&db.AdminSession{}).Count(&count)
	if count != 1 {
		t.Errorf("expected 1 session in db, got %d", count)
	}
}

func TestAdminLogin_MethodNotAllowed(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	req, _ := http.NewRequest("PUT", baseURL+"/admin/login", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// admin logout
// ---------------------------------------------------------------------------

func TestAdminLogout(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)
	cookie := adminLogin(t, svc)

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	req, _ := http.NewRequest("GET", baseURL+"/admin/logout", nil)
	req.AddCookie(&http.Cookie{Name: "admin_session", Value: cookie})
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusFound {
		t.Errorf("expected 302, got %d", resp.StatusCode)
	}

	for _, c := range resp.Cookies() {
		if c.Name == "admin_session" && c.MaxAge == -1 {
			break
		}
	}

	var count int64
	svc.db.Model(&db.AdminSession{}).Count(&count)
	if count != 0 {
		t.Errorf("expected session deleted, got %d sessions", count)
	}
}

// ---------------------------------------------------------------------------
// admin balance endpoint
// ---------------------------------------------------------------------------

func TestAdminGetBalance(t *testing.T) {
	svc, _ := testServiceFull(t)
	cookie := adminLogin(t, svc)

	r := httptest.NewRequest("GET", "/admin/balance", nil)
	r.AddCookie(&http.Cookie{Name: "admin_session", Value: cookie})
	w := httptest.NewRecorder()

	svc.adminGetBalanceHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	resp := decodeJSON(t, w.Body)
	if resp["trusted"].(float64) != 10.0 {
		t.Errorf("expected trusted=10, got %v", resp["trusted"])
	}
	if resp["total"].(float64) != 11.5 {
		t.Errorf("expected total=11.5, got %v", resp["total"])
	}
}

// ---------------------------------------------------------------------------
// admin get new address
// ---------------------------------------------------------------------------

func TestAdminGetNewAddress(t *testing.T) {
	svc, _ := testServiceFull(t)

	r := httptest.NewRequest("GET", "/admin/getnewaddress", nil)
	w := httptest.NewRecorder()
	svc.adminGetNewAddressHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	resp := decodeJSON(t, w.Body)
	if resp["address"] == nil || resp["address"].(string) == "" {
		t.Error("expected address in response")
	}
}

// ---------------------------------------------------------------------------
// admin send funds
// ---------------------------------------------------------------------------

func TestAdminSendFunds_Success(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{
		"address": "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount":  0.5,
	})
	r := httptest.NewRequest("POST", "/admin/sendfunds", body)
	w := httptest.NewRecorder()
	svc.adminSendFundsHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	resp := decodeJSON(t, w.Body)
	if resp["success"] != true {
		t.Error("expected success=true")
	}
	if resp["txid"] == nil || resp["txid"].(string) == "" {
		t.Error("expected txid in response")
	}
}

func TestAdminSendFunds_MethodNotAllowed(t *testing.T) {
	svc, _ := testServiceFull(t)

	r := httptest.NewRequest("GET", "/admin/sendfunds", nil)
	w := httptest.NewRecorder()
	svc.adminSendFundsHandler(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestAdminSendFunds_InvalidJSON(t *testing.T) {
	svc, _ := testServiceFull(t)

	r := httptest.NewRequest("POST", "/admin/sendfunds", strings.NewReader("nope"))
	w := httptest.NewRecorder()
	svc.adminSendFundsHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAdminSendFunds_InvalidAddress(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{"address": "invalid_addr", "amount": 0.1})
	r := httptest.NewRequest("POST", "/admin/sendfunds", body)
	w := httptest.NewRecorder()
	svc.adminSendFundsHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAdminSendFunds_ZeroAmount(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{
		"address": "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount":  0,
	})
	r := httptest.NewRequest("POST", "/admin/sendfunds", body)
	w := httptest.NewRecorder()
	svc.adminSendFundsHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAdminSendFunds_InsufficientBalance(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{
		"address": "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount":  99999.0,
	})
	r := httptest.NewRequest("POST", "/admin/sendfunds", body)
	w := httptest.NewRecorder()
	svc.adminSendFundsHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
	resp := decodeJSON(t, w.Body)
	if !strings.Contains(resp["error"].(string), "Insufficient") {
		t.Errorf("expected insufficient balance error, got: %s", resp["error"])
	}
}

// ---------------------------------------------------------------------------
// admin UTXOs
// ---------------------------------------------------------------------------

func TestAdminGetUTXOs(t *testing.T) {
	svc, _ := testServiceFull(t)

	r := httptest.NewRequest("GET", "/admin/utxos", nil)
	w := httptest.NewRecorder()
	svc.adminGetUTXOsHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	resp := decodeJSON(t, w.Body)
	utxos, ok := resp["utxos"].([]any)
	if !ok {
		t.Fatal("expected utxos array")
	}
	if len(utxos) != 3 {
		t.Errorf("expected 3 utxos, got %d", len(utxos))
	}

	first := utxos[0].(map[string]any)
	second := utxos[1].(map[string]any)
	if first["confirmations"].(float64) > second["confirmations"].(float64) {
		t.Error("utxos should be sorted by confirmations ascending")
	}
}

// ---------------------------------------------------------------------------
// admin consolidate
// ---------------------------------------------------------------------------

func TestAdminConsolidate_Success(t *testing.T) {
	svc, _ := testServiceFull(t)

	body := jsonBody(map[string]any{})
	r := httptest.NewRequest("POST", "/admin/consolidate", body)
	w := httptest.NewRecorder()
	svc.adminConsolidateUTXOsHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	resp := decodeJSON(t, w.Body)
	if resp["success"] != true {
		t.Errorf("expected success=true, got %v", resp)
	}
	if resp["count"].(float64) != 2 {
		t.Errorf("expected 2 consolidated utxos, got %v", resp["count"])
	}
}

func TestAdminConsolidate_MethodNotAllowed(t *testing.T) {
	svc, _ := testServiceFull(t)

	r := httptest.NewRequest("GET", "/admin/consolidate", nil)
	w := httptest.NewRecorder()
	svc.adminConsolidateUTXOsHandler(w, r)

	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", w.Code)
	}
}

func TestAdminConsolidate_InvalidJSON(t *testing.T) {
	svc, _ := testServiceFull(t)

	r := httptest.NewRequest("POST", "/admin/consolidate", strings.NewReader("bad"))
	w := httptest.NewRecorder()
	svc.adminConsolidateUTXOsHandler(w, r)

	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", w.Code)
	}
}

func TestAdminConsolidate_NoSmallUTXOs(t *testing.T) {
	mock := newMockRPC()
	mock.handlers["listunspent"] = func(_ json.RawMessage) (any, *rpcErr) {
		return []btc.UTXO{
			{TxID: "aaa", Amount: 5.0, Confirmations: 10, Spendable: true},
		}, nil
	}
	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)

	body := jsonBody(map[string]any{})
	r := httptest.NewRequest("POST", "/admin/consolidate", body)
	w := httptest.NewRecorder()
	svc.adminConsolidateUTXOsHandler(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	resp := decodeJSON(t, w.Body)
	if resp["message"] == nil || !strings.Contains(resp["message"].(string), "No UTXOs") {
		t.Errorf("expected skip message, got: %v", resp)
	}
}

func TestAdminConsolidate_BelowMinUTXOs(t *testing.T) {
	mock := newMockRPC()
	mock.handlers["listunspent"] = func(_ json.RawMessage) (any, *rpcErr) {
		return []btc.UTXO{
			{TxID: "aaa", Amount: 0.0005, Confirmations: 10, Spendable: true},
		}, nil
	}
	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)

	body := jsonBody(map[string]any{})
	r := httptest.NewRequest("POST", "/admin/consolidate", body)
	w := httptest.NewRecorder()
	svc.adminConsolidateUTXOsHandler(w, r)

	resp := decodeJSON(t, w.Body)
	if resp["message"] == nil || !strings.Contains(resp["message"].(string), "need at least") {
		t.Errorf("expected min utxo skip, got: %v", resp)
	}
}

// ---------------------------------------------------------------------------
// ConsolidateUTXOs logic
// ---------------------------------------------------------------------------

func TestConsolidateUTXOs_SkipsDust(t *testing.T) {
	mock := newMockRPC()
	mock.handlers["listunspent"] = func(_ json.RawMessage) (any, *rpcErr) {
		return []btc.UTXO{
			{TxID: "dust1", Amount: 0.000001, Spendable: true},
			{TxID: "dust2", Amount: 0.000002, Spendable: true},
			{TxID: "ok1", Amount: 0.0005, Spendable: true},
			{TxID: "ok2", Amount: 0.0003, Spendable: true},
		}, nil
	}
	mock.handlers["getnewaddress"] = func(_ json.RawMessage) (any, *rpcErr) {
		return "tb1qconsolidated", nil
	}
	mock.handlers["createrawtransaction"] = func(_ json.RawMessage) (any, *rpcErr) { return "raw", nil }
	mock.handlers["signrawtransactionwithwallet"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{"hex": "signed", "complete": true}, nil
	}
	mock.handlers["sendrawtransaction"] = func(_ json.RawMessage) (any, *rpcErr) { return "txid123", nil }

	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)

	result, err := svc.ConsolidateUTXOs()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Count != 2 {
		t.Errorf("expected 2 UTXOs consolidated (dust excluded), got %d", result.Count)
	}
}

func TestConsolidateUTXOs_SkipsUnspendable(t *testing.T) {
	mock := newMockRPC()
	mock.handlers["listunspent"] = func(_ json.RawMessage) (any, *rpcErr) {
		return []btc.UTXO{
			{TxID: "a", Amount: 0.0005, Spendable: false},
			{TxID: "b", Amount: 0.0003, Spendable: false},
		}, nil
	}
	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)

	result, err := svc.ConsolidateUTXOs()
	if err != nil {
		t.Fatal(err)
	}
	if result.SkipReason == "" {
		t.Error("expected skip due to unspendable UTXOs")
	}
}

func TestConsolidateUTXOs_RespectsMax(t *testing.T) {
	mock := newMockRPC()
	mock.handlers["listunspent"] = func(_ json.RawMessage) (any, *rpcErr) {
		var utxos []btc.UTXO
		for i := range 10 {
			utxos = append(utxos, btc.UTXO{
				TxID: fmt.Sprintf("tx%d", i), Amount: 0.0005, Spendable: true,
			})
		}
		return utxos, nil
	}
	mock.handlers["getnewaddress"] = func(_ json.RawMessage) (any, *rpcErr) { return "tb1q", nil }
	mock.handlers["createrawtransaction"] = func(_ json.RawMessage) (any, *rpcErr) { return "r", nil }
	mock.handlers["signrawtransactionwithwallet"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{"hex": "s", "complete": true}, nil
	}
	mock.handlers["sendrawtransaction"] = func(_ json.RawMessage) (any, *rpcErr) { return "txid", nil }

	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)
	svc.cfg.MaxConsolidationUTXOs = 3

	result, err := svc.ConsolidateUTXOs()
	if err != nil {
		t.Fatal(err)
	}
	if result.Count != 3 {
		t.Errorf("expected max 3, got %d", result.Count)
	}
}

// ---------------------------------------------------------------------------
// batch processor
// ---------------------------------------------------------------------------

func TestProcessBatch_NoPending(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.processBatch()

	var count int64
	svc.db.Model(&db.Transaction{}).Count(&count)
	if count != 0 {
		t.Errorf("expected no transactions, got %d", count)
	}
}

func TestProcessBatch_SendsTransactions(t *testing.T) {
	svc, _ := testServiceFull(t)

	svc.db.Create(&db.Transaction{
		Address:   "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		IPAddress: "1.2.3.4",
		AmountBTC: 0.05,
		Status:    db.TxnStatusPending,
	})
	svc.db.Create(&db.Transaction{
		Address:   "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		IPAddress: "1.2.3.5",
		AmountBTC: 0.03,
		Status:    db.TxnStatusPending,
	})

	svc.processBatch()

	var txns []db.Transaction
	svc.db.Find(&txns)

	for _, tx := range txns {
		if tx.Status != db.TxnStatusBroadcast {
			t.Errorf("expected broadcast, got %s for tx %d", tx.Status, tx.ID)
		}
		if tx.OnchainTxnID == "" {
			t.Errorf("expected onchain txid for tx %d", tx.ID)
		}
	}
}

func TestProcessBatch_InsufficientBalance(t *testing.T) {
	mock := newMockRPC()
	mock.handlers["getbalances"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{
			"mine": map[string]any{"trusted": 0.0001, "untrusted_pending": 0.0, "immature": 0.0},
		}, nil
	}
	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)

	svc.db.Create(&db.Transaction{
		Address:   "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		AmountBTC: 100.0,
		Status:    db.TxnStatusPending,
	})

	svc.processBatch()

	var tx db.Transaction
	svc.db.First(&tx)
	if tx.Status != db.TxnStatusPending {
		t.Errorf("expected still pending (insufficient balance), got %s", tx.Status)
	}
}

func TestProcessBatch_RPCFailure(t *testing.T) {
	mock := newMockRPC()
	mock.handlers["getbalances"] = func(_ json.RawMessage) (any, *rpcErr) {
		return map[string]any{
			"mine": map[string]any{"trusted": 100.0, "untrusted_pending": 0.0, "immature": 0.0},
		}, nil
	}
	mock.handlers["createrawtransaction"] = func(_ json.RawMessage) (any, *rpcErr) {
		return nil, &rpcErr{Code: -1, Message: "wallet locked"}
	}
	rpcServer := httptest.NewServer(mock)
	t.Cleanup(rpcServer.Close)
	svc := testService(t, rpcServer)

	svc.db.Create(&db.Transaction{
		Address:   "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		AmountBTC: 0.05,
		Status:    db.TxnStatusPending,
	})

	svc.processBatch()

	var tx db.Transaction
	svc.db.First(&tx)
	if tx.Status != db.TxnStatusFailed {
		t.Errorf("expected failed after RPC error, got %s", tx.Status)
	}
	if tx.ErrorMsg == "" {
		t.Error("expected error message to be recorded")
	}
}

// ---------------------------------------------------------------------------
// metrics endpoint
// ---------------------------------------------------------------------------

func TestMetricsHandler(t *testing.T) {
	svc, _ := testServiceFull(t)

	handler := svc.MetricsHandler()
	r := httptest.NewRequest("GET", "/metrics", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", w.Code)
	}

	body := w.Body.String()
	if !strings.Contains(body, "faucet_wallet_balance_btc") {
		t.Error("expected faucet_wallet_balance_btc metric")
	}
	if !strings.Contains(body, "faucet_transactions_count") {
		t.Error("expected faucet_transactions_count metric")
	}
	if !strings.Contains(body, "faucet_bitcoin_healthy") {
		t.Error("expected faucet_bitcoin_healthy metric")
	}
}

// ---------------------------------------------------------------------------
// metricsMiddleware
// ---------------------------------------------------------------------------

func TestMetricsMiddleware(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("ok"))
	})

	handler := metricsMiddleware(inner)
	r := httptest.NewRequest("GET", "/test-path", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// responseWriter wrapper
// ---------------------------------------------------------------------------

func TestResponseWriter_DefaultStatusCode(t *testing.T) {
	rw := &responseWriter{ResponseWriter: httptest.NewRecorder(), statusCode: http.StatusOK}
	if rw.statusCode != 200 {
		t.Errorf("expected default 200, got %d", rw.statusCode)
	}
	rw.WriteHeader(404)
	if rw.statusCode != 404 {
		t.Errorf("expected 404, got %d", rw.statusCode)
	}
}

// ---------------------------------------------------------------------------
// full integration: submit -> batch process -> broadcast
// ---------------------------------------------------------------------------

func TestIntegration_SubmitAndProcess(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}

	body := jsonBody(map[string]any{
		"address":      "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount_range": 1,
	})
	r := httptest.NewRequest("POST", "/api/submit", body)
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	svc.submitHandler(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("submit failed: %d %s", w.Code, w.Body.String())
	}

	var pending int64
	svc.db.Model(&db.Transaction{}).Where("status = ?", db.TxnStatusPending).Count(&pending)
	if pending != 1 {
		t.Fatalf("expected 1 pending, got %d", pending)
	}

	svc.processBatch()

	var broadcast int64
	svc.db.Model(&db.Transaction{}).Where("status = ?", db.TxnStatusBroadcast).Count(&broadcast)
	if broadcast != 1 {
		t.Errorf("expected 1 broadcast after batch, got %d", broadcast)
	}

	var tx db.Transaction
	svc.db.First(&tx)
	if tx.OnchainTxnID == "" {
		t.Error("expected onchain txid after broadcast")
	}
	if tx.AmountBTC < 0.001 || tx.AmountBTC > 0.009 {
		t.Errorf("amount %.8f should be in range 1", tx.AmountBTC)
	}
}

// ---------------------------------------------------------------------------
// full server routing integration
// ---------------------------------------------------------------------------

func TestFullServer_HealthEndpoint(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	resp, err := http.Get(baseURL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("expected 'ok', got %q", string(body))
	}
}

func TestFullServer_SubmitEndpoint(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	body := jsonBody(map[string]any{
		"address":      "tb1qw508d6qejxtdg4y5r3zarvary0c5xw7kxpjzsx",
		"amount_range": 2,
	})
	resp, err := http.Post(baseURL+"/api/submit", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Errorf("expected 200, got %d: %s", resp.StatusCode, string(b))
	}
}

func TestFullServer_AdminAuthenticatedFlow(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.1/32")}
	baseURL := startTestServer(t, svc)

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}

	loginResp, err := client.PostForm(baseURL+"/admin/login", url.Values{
		"password": {"testpass123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	loginResp.Body.Close()

	var sessionCookie *http.Cookie
	for _, c := range loginResp.Cookies() {
		if c.Name == "admin_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie after login")
	}

	balReq, _ := http.NewRequest("GET", baseURL+"/admin/balance", nil)
	balReq.AddCookie(sessionCookie)
	balResp, err := client.Do(balReq)
	if err != nil {
		t.Fatal(err)
	}
	defer balResp.Body.Close()

	if balResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for authenticated balance, got %d", balResp.StatusCode)
	}

	resp := decodeJSON(t, balResp.Body)
	if resp["trusted"].(float64) != 10.0 {
		t.Errorf("expected trusted=10, got %v", resp["trusted"])
	}
}

// ---------------------------------------------------------------------------
// admin CIDR allowlist integration
// ---------------------------------------------------------------------------

func TestFullServer_AdminCIDRAllowlist(t *testing.T) {
	svc, _ := testServiceFull(t)
	svc.cfg.AdminAllowlist = []net.IPNet{parseCIDR("127.0.0.0/8")}
	baseURL := startTestServer(t, svc)

	resp, err := http.Get(baseURL + "/admin/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 (127.0.0.1 in 127.0.0.0/8), got %d", resp.StatusCode)
	}
}
