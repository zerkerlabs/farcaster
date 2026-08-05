//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethcrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/zerkerlabs/gateway/facilitator/internal/account"
	"github.com/zerkerlabs/gateway/facilitator/internal/chain"
	"github.com/zerkerlabs/gateway/facilitator/internal/guardrail"
	"github.com/zerkerlabs/gateway/facilitator/internal/mtls/mtlstest"
	"github.com/zerkerlabs/gateway/facilitator/internal/settle"
	"github.com/zerkerlabs/gateway/facilitator/internal/settlement"
	"github.com/zerkerlabs/gateway/facilitator/internal/verify"
	"github.com/zerkerlabs/gateway/x402types"
)

// e2eConfig is the ops-provided environment the live settle needs.
type e2eConfig struct {
	rpcURL       string
	network      string
	usdc         string
	assetName    string
	assetVersion string
	payTo        string
	value        string
	chainID      int64
	gasKeyHex    string
	payerKeyHex  string
}

// loadE2EConfig reads the FACILITATOR_E2E_* env; it skips the test (rather than
// failing) when any is absent, so `-tags e2e` runs are a no-op without a
// provisioned testnet.
func loadE2EConfig(t *testing.T) e2eConfig {
	t.Helper()
	env := map[string]string{
		"FACILITATOR_E2E_RPC_URL":       "",
		"FACILITATOR_E2E_NETWORK":       "",
		"FACILITATOR_E2E_USDC_ADDRESS":  "",
		"FACILITATOR_E2E_ASSET_NAME":    "",
		"FACILITATOR_E2E_ASSET_VERSION": "",
		"FACILITATOR_E2E_PAYTO":         "",
		"FACILITATOR_E2E_VALUE":         "",
		"FACILITATOR_E2E_CHAIN_ID":      "",
		"FACILITATOR_E2E_GAS_KEY":       "",
		"FACILITATOR_E2E_PAYER_KEY":     "",
	}
	var missing []string
	for k := range env {
		v := strings.TrimSpace(os.Getenv(k))
		if v == "" {
			missing = append(missing, k)
		}
		env[k] = v
	}
	if len(missing) > 0 {
		t.Skipf("Base Sepolia e2e skipped — set all FACILITATOR_E2E_* vars (needs a funded gas wallet + Sepolia RPC + a USDC-holding payer). Missing: %s", strings.Join(missing, ", "))
	}

	chainID, err := strconv.ParseInt(env["FACILITATOR_E2E_CHAIN_ID"], 10, 64)
	if err != nil {
		t.Fatalf("FACILITATOR_E2E_CHAIN_ID is not an integer: %v", err)
	}
	return e2eConfig{
		rpcURL:       env["FACILITATOR_E2E_RPC_URL"],
		network:      env["FACILITATOR_E2E_NETWORK"],
		usdc:         env["FACILITATOR_E2E_USDC_ADDRESS"],
		assetName:    env["FACILITATOR_E2E_ASSET_NAME"],
		assetVersion: env["FACILITATOR_E2E_ASSET_VERSION"],
		payTo:        env["FACILITATOR_E2E_PAYTO"],
		value:        env["FACILITATOR_E2E_VALUE"],
		chainID:      chainID,
		gasKeyHex:    env["FACILITATOR_E2E_GAS_KEY"],
		payerKeyHex:  env["FACILITATOR_E2E_PAYER_KEY"],
	}
}

// TestSepoliaSettleEndToEnd proves the whole surface: a valid mTLS client POSTs
// a real, payer-signed EIP-3009 authorization to /settle; the facilitator
// re-verifies, submits transferWithAuthorization on Base Sepolia, waits for
// first-block inclusion, and returns the tx hash — and a settled row is
// recorded. Requires a funded testnet gas wallet + a USDC-holding payer.
func TestSepoliaSettleEndToEnd(t *testing.T) {
	cfg := loadE2EConfig(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Chain client over the real Sepolia RPC, signing from the funded gas key.
	gasKey, err := ethcrypto.HexToECDSA(strings.TrimPrefix(cfg.gasKeyHex, "0x"))
	if err != nil {
		t.Fatalf("parse gas key: %v", err)
	}
	client, err := chain.Dial(ctx, cfg.rpcURL, chain.NewLocalSigner(gasKey), big.NewInt(cfg.chainID), chain.Config{
		USDC: ethcommon.HexToAddress(cfg.usdc),
	})
	if err != nil {
		t.Fatalf("dial chain client: %v", err)
	}

	// Real settle handler: real re-verify, in-memory store, generous guardrails.
	store := settlement.NewMemoryStore()
	generous, _ := new(big.Int).SetString("1000000000000", 10)
	handler := settle.NewHandler(settle.Config{
		Policy: verify.Policy{Kinds: []verify.Kind{{
			Network: cfg.network, Asset: cfg.usdc, ChainID: cfg.chainID,
			AssetName: cfg.assetName, AssetVersion: cfg.assetVersion,
		}}},
		Submitters: map[string]settle.Submitter{cfg.network: client},
		Store:      store,
		GuardrailDefaults: guardrail.Limits{
			AllowedKinds:    []guardrail.Kind{{Network: cfg.network, Asset: cfg.usdc}},
			MaxSettleAmount: generous,
			DailyCeiling:    generous,
		},
		Logger: logger,
	})

	// Stand up the facilitator behind real mTLS (account middleware + handler).
	ca := mtlstest.NewCA(t)
	clientCert := ca.IssueLeaf(t, "e2e-client", x509.ExtKeyUsageClientAuth)
	accounts := account.NewMemoryStore()
	acct := &account.Account{Name: "e2e-operator", CertFingerprint: account.Fingerprint(clientCert.Cert), Active: true}
	if err := accounts.Create(ctx, acct); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(ca.Cert)
	srv := httptest.NewUnstartedServer(account.Middleware(accounts, logger)(handler))
	srv.TLS = &tls.Config{
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  caPool,
		MinVersion: tls.VersionTLS12,
	}
	srv.StartTLS()
	defer srv.Close()

	httpClient := srv.Client()
	httpClient.Transport.(*http.Transport).TLSClientConfig.Certificates = []tls.Certificate{clientCert.TLSCert}

	// Craft a real payer-signed EIP-3009 authorization.
	payerKey, err := ethcrypto.HexToECDSA(strings.TrimPrefix(cfg.payerKeyHex, "0x"))
	if err != nil {
		t.Fatalf("parse payer key: %v", err)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatalf("generate nonce: %v", err)
	}
	nonceHex := "0x" + hex.EncodeToString(nonce[:])
	authz := x402types.Authorization{
		From:        ethcrypto.PubkeyToAddress(payerKey.PublicKey).Hex(),
		To:          cfg.payTo,
		Value:       cfg.value,
		ValidAfter:  "0",
		ValidBefore: strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		Nonce:       nonceHex,
	}
	signature := signEIP3009(t, payerKey, authz, cfg)

	reqBody, err := json.Marshal(x402types.SettleRequest{
		X402Version: 1,
		PaymentPayload: x402types.Payment{
			Network: cfg.network, Scheme: "exact",
			Payload: x402types.PaymentPayload{Signature: signature, Authorization: authz},
		},
		PaymentRequirements: x402types.PaymentRequirements{
			Network: cfg.network, Asset: cfg.usdc, MaxAmountRequired: cfg.value,
			PayTo: cfg.payTo, Scheme: "exact", MaxTimeoutSeconds: 120,
		},
	})
	if err != nil {
		t.Fatalf("marshal settle request: %v", err)
	}

	resp, err := httpClient.Post(srv.URL+"/settle", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatalf("POST /settle: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", resp.StatusCode, body)
	}
	var settleResp x402types.SettleResponse
	if err := json.Unmarshal(body, &settleResp); err != nil {
		t.Fatalf("decode SettleResponse: %v (body: %s)", err, body)
	}
	if !settleResp.Success {
		t.Fatalf("settle failed: %q", settleResp.ErrorReason)
	}
	if settleResp.Transaction == "" {
		t.Fatal("expected a transaction hash on a successful settle")
	}
	t.Logf("settled on %s: tx=%s payer=%s", cfg.network, settleResp.Transaction, settleResp.Payer)

	// The settlement row is recorded, settled, with the returned tx hash.
	row, err := store.Get(ctx, acct.ID, nonceHex)
	if err != nil {
		t.Fatalf("settlement row not recorded: %v", err)
	}
	if row.Status != settlement.StatusSettled {
		t.Fatalf("row status = %q, want settled", row.Status)
	}
	if row.TxHash != settleResp.Transaction {
		t.Fatalf("row tx = %q, want %q", row.TxHash, settleResp.Transaction)
	}
}

// signEIP3009 produces the payer's 65-byte EIP-712 signature over the EIP-3009
// TransferWithAuthorization, using the same digest the facilitator's verifier
// recomputes (asset domain name/version/chainId/contract).
func signEIP3009(t *testing.T, key *ecdsa.PrivateKey, authz x402types.Authorization, cfg e2eConfig) string {
	t.Helper()
	domainType := ethcrypto.Keccak256([]byte("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"))
	domainSeparator := ethcrypto.Keccak256(
		domainType,
		ethcrypto.Keccak256([]byte(cfg.assetName)),
		ethcrypto.Keccak256([]byte(cfg.assetVersion)),
		uint256Word(t, strconv.FormatInt(cfg.chainID, 10)),
		addressWord(t, cfg.usdc),
	)
	typeHash := ethcrypto.Keccak256([]byte("TransferWithAuthorization(address from,address to,uint256 value,uint256 validAfter,uint256 validBefore,bytes32 nonce)"))
	structHash := ethcrypto.Keccak256(
		typeHash,
		addressWord(t, authz.From),
		addressWord(t, authz.To),
		uint256Word(t, authz.Value),
		uint256Word(t, authz.ValidAfter),
		uint256Word(t, authz.ValidBefore),
		bytes32Word(t, authz.Nonce),
	)
	digest := ethcrypto.Keccak256([]byte{0x19, 0x01}, domainSeparator, structHash)
	sig, err := ethcrypto.Sign(digest, key)
	if err != nil {
		t.Fatalf("sign authorization: %v", err)
	}
	return "0x" + hex.EncodeToString(sig)
}

func addressWord(t *testing.T, addr string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(addr, "0x"))
	if err != nil || len(b) != 20 {
		t.Fatalf("invalid address %q", addr)
	}
	w := make([]byte, 32)
	copy(w[12:], b)
	return w
}

func uint256Word(t *testing.T, dec string) []byte {
	t.Helper()
	n, ok := new(big.Int).SetString(dec, 10)
	if !ok {
		t.Fatalf("invalid uint256 %q", dec)
	}
	w := make([]byte, 32)
	n.FillBytes(w)
	return w
}

func bytes32Word(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(strings.TrimPrefix(s, "0x"))
	if err != nil || len(b) != 32 {
		t.Fatalf("invalid bytes32 %q", s)
	}
	return b
}
