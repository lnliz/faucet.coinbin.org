package service

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

type BitcoinRPCConfig struct {
	Host     string
	User     string
	Password string
}

type BitcoinRPCClient struct {
	config     *BitcoinRPCConfig
	httpClient *http.Client
	wallet     string
}

type rpcRequest struct {
	Jsonrpc string `json:"jsonrpc"`
	ID      string `json:"id"`
	Method  string `json:"method"`
	Params  []any  `json:"params"`
}

type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
	ID     string          `json:"id"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type BlockchainInfo struct {
	Chain                string  `json:"chain"`
	Blocks               int64   `json:"blocks"`
	Headers              int64   `json:"headers"`
	BestBlockHash        string  `json:"bestblockhash"`
	Difficulty           float64 `json:"difficulty"`
	VerificationProgress float64 `json:"verificationprogress"`
	ChainWork            string  `json:"chainwork"`
	Pruned               bool    `json:"pruned"`
}

type WalletBalance struct {
	Trusted   float64 `json:"trusted"`
	Untrusted float64 `json:"untrusted_pending"`
	Immature  float64 `json:"immature"`
}

type Balances struct {
	Mine WalletBalance `json:"mine"`
}

const (
	dustLimitBTC = 0.00001 // 1000 sats

	feeSatsPerVBLowerLimit = 0.1
)

func NewBitcoinRPCClient(config *BitcoinRPCConfig) *BitcoinRPCClient {
	return &BitcoinRPCClient{
		config: config,
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
	}
}

func (c *BitcoinRPCClient) call(method string, params []any) (json.RawMessage, error) {
	reqBody := rpcRequest{
		Jsonrpc: "1.0",
		ID:      "faucet",
		Method:  method,
		Params:  params,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	url := fmt.Sprintf("http://%s/", c.config.Host)
	if c.wallet != "" {
		url = fmt.Sprintf("http://%s/wallet/%s", c.config.Host, c.wallet)
	}

	req, err := http.NewRequest("POST", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.SetBasicAuth(c.config.User, c.config.Password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("authentication failed (401) - check RPC user/password")
	}

	if resp.StatusCode == 403 {
		return nil, fmt.Errorf("forbidden (403) - check rpcallowip settings")
	}

	if resp.StatusCode != 200 {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, preview)
	}

	var rpcResp rpcResponse
	if err := json.Unmarshal(body, &rpcResp); err != nil {
		preview := string(body)
		if len(preview) > 200 {
			preview = preview[:200] + "..."
		}
		return nil, fmt.Errorf("failed to unmarshal response (HTTP %d): %w\nResponse preview: %s", resp.StatusCode, err, preview)
	}

	if rpcResp.Error != nil {
		return nil, fmt.Errorf("RPC error %d: %s", rpcResp.Error.Code, rpcResp.Error.Message)
	}

	//	log.Printf("RPC [method=%s] response: %+v", method, string(rpcResp.Result))

	return rpcResp.Result, nil
}

func (c *BitcoinRPCClient) SendToAddressWithOpReturn(address string, amountBTC float64, feeRateSatsPerVB float64, opReturnData string) (string, error) {
	log.Printf("Sending %.8f btc to %s  [fees=%.8f sats/vb]", amountBTC, address, feeRateSatsPerVB)
	if amountBTC < dustLimitBTC {
		return "", fmt.Errorf("Amount too low")
	}

	outputs := map[string]string{
		address: fmt.Sprintf("%.8f", amountBTC),
	}

	if len(opReturnData) > 0 {
		outputs["data"] = hex.EncodeToString([]byte(opReturnData))
	}

	createParams := []any{[]any{}, outputs}
	rawTx, err := c.call("createrawtransaction", createParams)
	if err != nil {
		return "", fmt.Errorf("createrawtransaction failed: %w", err)
	}

	var rawTxHex string
	if err := json.Unmarshal(rawTx, &rawTxHex); err != nil {
		return "", fmt.Errorf("failed to unmarshal raw tx: %w", err)
	}

	fundParams := []any{
		rawTxHex,
	}

	if feeRateSatsPerVB > 0 {
		fundParams = append(fundParams, map[string]string{
			"fee_rate": fmt.Sprintf("%.8f", feeRateSatsPerVB),
		})
	}

	fundedTx, err := c.call("fundrawtransaction", fundParams)
	if err != nil {
		return "", fmt.Errorf("fundrawtransaction failed: %w", err)
	}

	var fundResult struct {
		Hex string  `json:"hex"`
		Fee float64 `json:"fee"`
	}
	if err := json.Unmarshal(fundedTx, &fundResult); err != nil {
		return "", fmt.Errorf("failed to unmarshal funded tx: %w", err)
	}

	signParams := []any{fundResult.Hex}
	signedTx, err := c.call("signrawtransactionwithwallet", signParams)
	if err != nil {
		return "", fmt.Errorf("signrawtransactionwithwallet failed: %w", err)
	}

	var signResult struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := json.Unmarshal(signedTx, &signResult); err != nil {
		return "", fmt.Errorf("failed to unmarshal signed tx: %w", err)
	}

	if !signResult.Complete {
		return "", fmt.Errorf("transaction signing incomplete")
	}

	sendParams := []any{signResult.Hex}
	txidResult, err := c.call("sendrawtransaction", sendParams)
	if err != nil {
		return "", fmt.Errorf("sendrawtransaction failed: %w", err)
	}

	var txid string
	if err := json.Unmarshal(txidResult, &txid); err != nil {
		return "", fmt.Errorf("failed to unmarshal txid: %w", err)
	}

	return txid, nil
}

func (c *BitcoinRPCClient) GetBlockCount() (int64, error) {
	result, err := c.call("getblockcount", []any{})
	if err != nil {
		return 0, err
	}

	var count int64
	if err := json.Unmarshal(result, &count); err != nil {
		return 0, fmt.Errorf("failed to unmarshal block count: %w", err)
	}

	return count, nil
}

func (c *BitcoinRPCClient) GetBlockchainInfo() (*BlockchainInfo, error) {
	result, err := c.call("getblockchaininfo", []any{})
	if err != nil {
		return nil, err
	}

	var info BlockchainInfo
	if err := json.Unmarshal(result, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal blockchain info: %w", err)
	}

	return &info, nil
}

func (c *BitcoinRPCClient) ListWallets() ([]string, error) {
	result, err := c.call("listwallets", []any{})
	if err != nil {
		return nil, err
	}

	var wallets []string
	if err := json.Unmarshal(result, &wallets); err != nil {
		return nil, fmt.Errorf("failed to unmarshal wallets: %w", err)
	}

	return wallets, nil
}

func (c *BitcoinRPCClient) LoadWallet(walletName string) error {
	_, err := c.call("loadwallet", []any{walletName})
	return err
}

func (c *BitcoinRPCClient) Consolidate(inputs []UTXO, totalAmountBTC float64, address string, opReturnData string) (string, error) {
	var txInputs []map[string]interface{}
	sort.Slice(inputs, func(i, j int) bool {
		return inputs[i].Amount > inputs[j].Amount
	})

	for _, input := range inputs {
		i := map[string]interface{}{
			"txid": input.TxID,
			"vout": input.Vout,
		}
		txInputs = append(txInputs, i)
	}

	numInputs := len(txInputs)
	numOutputs := 1
	if len(opReturnData) > 0 {
		numOutputs = 2
	}

	/*
	  fee calculation
	  - base: 10.5 vBytes
	  - per input: 148 vBytes (P2WPKH)
	  - per output: 31 vBytes (P2WPKH)
	  - fee rate: 0.15 sat/vB
	  - formula: (10.5 + inputs*148 + outputs*31) * 1 sat/vB
	*/
	estimatedVBytes := 10.5 + float64(numInputs)*148 + float64(numOutputs)*31.0
	feeRateSatPerVB := 0.15
	feeSats := estimatedVBytes * feeRateSatPerVB
	estimatedFeeBTC := feeSats / 100_000_000

	outputAmount := totalAmountBTC - estimatedFeeBTC
	if outputAmount <= 0 {
		return "", fmt.Errorf("total amount too small to cover fees")
	}

	outputs := map[string]string{
		address: fmt.Sprintf("%.8f", outputAmount),
	}

	if len(opReturnData) > 0 {
		outputs["data"] = hex.EncodeToString([]byte(opReturnData))
	}

	createParams := []any{txInputs, outputs}
	rawTx, err := c.call("createrawtransaction", createParams)
	if err != nil {
		return "", fmt.Errorf("createrawtransaction failed: %w", err)
	}

	var rawTxHex string
	if err := json.Unmarshal(rawTx, &rawTxHex); err != nil {
		return "", fmt.Errorf("failed to unmarshal raw tx: %w", err)
	}

	signParams := []any{rawTxHex}
	signedTx, err := c.call("signrawtransactionwithwallet", signParams)
	if err != nil {
		return "", fmt.Errorf("signrawtransactionwithwallet failed: %w", err)
	}

	var signResult struct {
		Hex      string `json:"hex"`
		Complete bool   `json:"complete"`
	}
	if err := json.Unmarshal(signedTx, &signResult); err != nil {
		return "", fmt.Errorf("failed to unmarshal signed tx: %w", err)
	}

	if !signResult.Complete {
		return "", fmt.Errorf("transaction signing incomplete")
	}

	sendParams := []any{signResult.Hex}
	txidResult, err := c.call("sendrawtransaction", sendParams)
	if err != nil {
		return "", fmt.Errorf("sendrawtransaction failed: %w", err)
	}

	var txid string
	if err := json.Unmarshal(txidResult, &txid); err != nil {
		return "", fmt.Errorf("failed to unmarshal txid: %w", err)
	}

	log.Printf(
		"[inputs: %d] [%.8f BTC] [estimated tx size: %.1f vB] [fee rate: %.3f sat/vB] [fee: %.0f sats] [output: %.8f] [addr: %s] [txid: %s]",
		len(inputs),
		totalAmountBTC, estimatedVBytes, feeRateSatPerVB, feeSats, outputAmount,
		address, txid,
	)

	return txid, nil
}

func (c *BitcoinRPCClient) WithWallet(walletName string) *BitcoinRPCClient {
	c.wallet = walletName
	return c
}

func (c *BitcoinRPCClient) GetNewAddress(label string, addressType string) (string, error) {
	params := []any{}
	if label != "" || addressType != "" {
		params = append(params, label)
		if addressType != "" {
			params = append(params, addressType)
		}
	}

	result, err := c.call("getnewaddress", params)
	if err != nil {
		return "", err
	}

	var address string
	if err := json.Unmarshal(result, &address); err != nil {
		return "", fmt.Errorf("failed to unmarshal address: %w", err)
	}

	return address, nil
}

func (c *BitcoinRPCClient) GetBalances() (*Balances, error) {
	result, err := c.call("getbalances", []any{})
	if err != nil {
		return nil, err
	}

	var balances Balances
	if err := json.Unmarshal(result, &balances); err != nil {
		return nil, fmt.Errorf("failed to unmarshal balances: %w", err)
	}

	return &balances, nil
}

type UTXO struct {
	TxID          string  `json:"txid"`
	Vout          int     `json:"vout"`
	Address       string  `json:"address"`
	Amount        float64 `json:"amount"`
	Confirmations int     `json:"confirmations"`
	Spendable     bool    `json:"spendable"`
	Solvable      bool    `json:"solvable"`
	Safe          bool    `json:"safe"`
}

func (c *BitcoinRPCClient) ListUnspent(minConf, maxConf int) ([]UTXO, error) {
	params := []any{minConf, maxConf}
	result, err := c.call("listunspent", params)
	if err != nil {
		return nil, err
	}

	var utxos []UTXO
	if err := json.Unmarshal(result, &utxos); err != nil {
		return nil, fmt.Errorf("failed to unmarshal utxos: %w", err)
	}

	return utxos, nil
}

var (
	bech32Regex = regexp.MustCompile(`^tb1[a-z0-9]{39,87}$`)
)

func ValidateSignetAddress(address string) error {
	address = strings.TrimSpace(address)

	if address == "" {
		return fmt.Errorf("address cannot be empty")
	}

	if strings.HasPrefix(address, "bc1") || strings.HasPrefix(address, "1") || strings.HasPrefix(address, "3") {
		return fmt.Errorf("mainnet address?")
	}

	if !bech32Regex.MatchString(address) {
		return fmt.Errorf("invalid signet address format, must be bech32 (tb1...)")
	}

	return nil
}
