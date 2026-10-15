package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

func TestCreateAccountsFreshKeysAndRefusesOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "seeds.json")
	if err := createAccounts("chain-1", 3, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("parent mode = %o, want 700", got)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("seed mode = %o, want 600", got)
	}
	var seeds seedFile
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &seeds); err != nil {
		t.Fatal(err)
	}
	if seeds.ChainID != "chain-1" || len(seeds.Accounts) != 3 {
		t.Fatalf("unexpected seed file: %+v", seeds)
	}
	seen := make(map[string]struct{})
	for _, account := range seeds.Accounts {
		if account.Mnemonic == "" || !strings.HasPrefix(account.Name, "bench-") {
			t.Fatalf("incomplete generated account: %+v", account)
		}
		if _, err := sdk.AccAddressFromBech32(account.Address); err != nil {
			t.Fatal(err)
		}
		if _, ok := seen[account.Address]; ok {
			t.Fatal("duplicate generated address")
		}
		seen[account.Address] = struct{}{}
	}
	if err := createAccounts("chain-1", 1, path); err == nil {
		t.Fatal("expected overwrite refusal")
	}
}

func TestFundAccountsHealthyAndSkipsPreexistingBalance(t *testing.T) {
	seeds := generatedSeeds(t, 2)
	var posts atomic.Int32
	var balances atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "" || r.URL.Path == "/"):
			_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"chain-1"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/faucet":
			_, _ = w.Write([]byte(`{"enabled":true,"chainId":"chain-1","denom":"urp1","amount":"1000000"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/cosmos/bank/"):
			if balances.Add(1) == 1 {
				_, _ = w.Write([]byte(`{"balance":{"denom":"urp1","amount":"0"}}`))
			} else {
				_, _ = w.Write([]byte(`{"balance":{"denom":"urp1","amount":"1000000"}}`))
			}
		case r.Method == http.MethodPost && r.URL.Path == "/faucet":
			posts.Add(1)
			_, _ = w.Write([]byte(`{"success":true,"tx_hash":"ABC"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	seedPath := writeSeeds(t, seeds)
	out := filepath.Join(t.TempDir(), "funding.json")
	if err := fundAccounts(context.Background(), fundConfig{seedPath: seedPath, rpcURL: server.URL, restURL: server.URL, faucetURL: server.URL + "/faucet", minimum: 1000000, timeout: time.Second, output: out}); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 1 {
		t.Fatalf("faucet posts = %d, want one claim for the underfunded account", posts.Load())
	}
	assertFundingAudit(t, out, 2)
}

func TestFundAccountsPreflightMismatchMakesNoPost(t *testing.T) {
	seeds := generatedSeeds(t, 1)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/faucet" {
			posts.Add(1)
		}
		if r.Method == http.MethodPost && (r.URL.Path == "" || r.URL.Path == "/") {
			_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"wrong"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"enabled":true,"chainId":"chain-1","denom":"urp1","amount":"1000000"}`))
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "funding.json")
	if err := fundAccounts(context.Background(), fundConfig{seedPath: writeSeeds(t, seeds), rpcURL: server.URL, restURL: server.URL, faucetURL: server.URL + "/faucet", minimum: 1000000, timeout: time.Second, output: out}); err == nil {
		t.Fatal("expected chain mismatch")
	}
	if posts.Load() != 0 {
		t.Fatal("posted despite failed preflight")
	}
	var audit fundingAudit
	data, readErr := os.ReadFile(out)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := json.Unmarshal(data, &audit); err != nil {
		t.Fatal(err)
	}
	if audit.Error == "" {
		t.Fatal("preflight failure was not recorded in audit")
	}
}

func TestFundAccountsAllPreexistingSkipsFaucetPreflight(t *testing.T) {
	seeds := generatedSeeds(t, 1)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "" || r.URL.Path == "/"):
			_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"chain-1"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
		case strings.HasPrefix(r.URL.Path, "/cosmos/bank/"):
			_, _ = w.Write([]byte(`{"balance":{"denom":"urp1","amount":"1000000"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/faucet":
			w.WriteHeader(http.StatusServiceUnavailable)
		case r.Method == http.MethodPost && r.URL.Path == "/faucet":
			posts.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "funding.json")
	if err := fundAccounts(context.Background(), fundConfig{seedPath: writeSeeds(t, seeds), rpcURL: server.URL, restURL: server.URL, faucetURL: server.URL + "/faucet", minimum: 1000000, timeout: time.Second, output: out}); err != nil {
		t.Fatal(err)
	}
	if posts.Load() != 0 {
		t.Fatalf("faucet posts = %d, want zero", posts.Load())
	}
}

func TestFundAccountsRefusesOutputAliasingSeedFile(t *testing.T) {
	seeds := generatedSeeds(t, 1)
	seedPath := writeSeeds(t, seeds)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"chain-1"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	if err := fundAccounts(context.Background(), fundConfig{seedPath: seedPath, rpcURL: server.URL, restURL: server.URL, faucetURL: server.URL + "/faucet", minimum: 1000000, timeout: time.Second, output: seedPath}); err == nil {
		t.Fatal("expected seed/output alias refusal")
	}
}

func TestFundAccountsRateLimitStopsAndWritesPartialAudit(t *testing.T) {
	seeds := generatedSeeds(t, 2)
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "" || r.URL.Path == "/"):
			_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"chain-1"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
		case r.URL.Path == "/faucet" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"enabled":true,"chainId":"chain-1","denom":"urp1","amount":"1000000"}`))
		case strings.HasPrefix(r.URL.Path, "/cosmos/bank/"):
			_, _ = w.Write([]byte(`{"balance":{"denom":"urp1","amount":"0"}}`))
		case r.URL.Path == "/faucet":
			posts.Add(1)
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "funding.json")
	if err := fundAccounts(context.Background(), fundConfig{seedPath: writeSeeds(t, seeds), rpcURL: server.URL, restURL: server.URL, faucetURL: server.URL + "/faucet", minimum: 1000000, timeout: time.Second, output: out}); err == nil {
		t.Fatal("expected rate-limit failure")
	}
	if posts.Load() != 1 {
		t.Fatalf("posts = %d, want one non-retried claim", posts.Load())
	}
	assertFundingAudit(t, out, 2)
}

func TestFundAccountsAcceptedClaimTimesOutAndWritesPartialAudit(t *testing.T) {
	seeds := generatedSeeds(t, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && (r.URL.Path == "" || r.URL.Path == "/"):
			_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"chain-1"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
		case r.URL.Path == "/faucet" && r.Method == http.MethodGet:
			_, _ = w.Write([]byte(`{"enabled":true,"chainId":"chain-1","denom":"urp1","amount":"1000000"}`))
		case strings.HasPrefix(r.URL.Path, "/cosmos/bank/"):
			_, _ = w.Write([]byte(`{"balance":{"denom":"urp1","amount":"0"}}`))
		case r.URL.Path == "/faucet":
			_, _ = w.Write([]byte(`{"success":true,"tx_hash":"ABC"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	out := filepath.Join(t.TempDir(), "funding.json")
	if err := fundAccounts(context.Background(), fundConfig{seedPath: writeSeeds(t, seeds), rpcURL: server.URL, restURL: server.URL, faucetURL: server.URL + "/faucet", minimum: 1000000, timeout: 20 * time.Millisecond, output: out}); err == nil {
		t.Fatal("expected balance timeout")
	}
	assertFundingAudit(t, out, 1)
}

func generatedSeeds(t *testing.T, count int) seedFile {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seeds.json")
	if err := createAccounts("chain-1", count, path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var seeds seedFile
	if err := json.Unmarshal(data, &seeds); err != nil {
		t.Fatal(err)
	}
	return seeds
}

func writeSeeds(t *testing.T, seeds seedFile) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "seeds.json")
	data, err := json.Marshal(seeds)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertFundingAudit(t *testing.T, path string, count int) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var audit fundingAudit
	if err := json.Unmarshal(data, &audit); err != nil {
		t.Fatal(err)
	}
	if len(audit.Accounts) != count {
		t.Fatalf("audit accounts = %d, want %d", len(audit.Accounts), count)
	}
	if strings.Contains(string(data), "mnemonic") || strings.Contains(string(data), "tx_hash") {
		t.Fatal("audit contains secret or unnecessary transaction data")
	}
}
