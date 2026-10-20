package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	"github.com/cosmos/cosmos-sdk/crypto/keys/secp256k1"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

func TestMain(m *testing.M) {
	configurePrefixes()
	os.Exit(m.Run())
}

func TestRunFixtureBindsSignedMultisendAndPersistsResult(t *testing.T) {
	for _, workload := range []struct {
		name           string
		ops, envelopes int
		duration       time.Duration
	}{
		{"duration", 2, 0, 20 * time.Millisecond},
		{"finite_6137", 6137, 3, 2 * time.Second},
	} {
		t.Run(workload.name, func(t *testing.T) {
			seeds := fixtureSeeds(t, 2)
			var mu sync.Mutex
			var submitted []string
			sequences := map[string]uint64{}
			byHash := map[string][]byte{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/cosmos/auth/") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"account":{"account_number":"1","sequence":"0"}}`))
					return
				}
				var req struct {
					Method string `json:"method"`
					Params struct {
						Tx   string `json:"tx"`
						Hash string `json:"hash"`
					} `json:"params"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				w.Header().Set("Content-Type", "application/json")
				mu.Lock()
				defer mu.Unlock()
				switch req.Method {
				case "status":
					_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"fixture-1"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
				case "broadcast_tx_sync":
					submitted = append(submitted, req.Params.Tx)
					raw, _ := base64.StdEncoding.DecodeString(req.Params.Tx)
					var wire txtypes.TxRaw
					var body txtypes.TxBody
					var auth txtypes.AuthInfo
					if err := wire.Unmarshal(raw); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if err := body.Unmarshal(wire.BodyBytes); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if err := auth.Unmarshal(wire.AuthInfoBytes); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if len(body.Messages) != 1 || body.Messages[0].TypeUrl != "/cosmos.bank.v1beta1.MsgMultiSend" || len(auth.SignerInfos) != 1 || len(wire.Signatures) != 1 {
						t.Error("expected one signed bank multisend")
						w.WriteHeader(400)
						return
					}
					var msg banktypes.MsgMultiSend
					if err := msg.Unmarshal(body.Messages[0].Value); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if len(msg.Inputs) != 1 || len(msg.Outputs) != workload.ops || msg.Inputs[0].Coins.AmountOf("urp1").Int64() != int64(workload.ops) || auth.Fee.Amount.AmountOf("urp1").Int64() != 2 {
						t.Error("incorrect batch output or fee accounting")
						w.WriteHeader(400)
						return
					}
					for _, out := range msg.Outputs {
						if out.Coins.AmountOf("urp1").Int64() != 1 {
							t.Error("output must transfer one urp1")
							w.WriteHeader(400)
							return
						}
					}
					signer := msg.Inputs[0].Address
					if auth.SignerInfos[0].Sequence != sequences[signer] {
						t.Error("incorrect account sequence")
						w.WriteHeader(400)
						return
					}
					sequences[signer]++
					var pub secp256k1.PubKey
					if err := pub.Unmarshal(auth.SignerInfos[0].PublicKey.Value); err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					signDoc := txtypes.SignDoc{BodyBytes: wire.BodyBytes, AuthInfoBytes: wire.AuthInfoBytes, ChainId: "fixture-1", AccountNumber: 1}
					signBytes, err := signDoc.Marshal()
					if err != nil || sdk.AccAddress(pub.Address()).String() != signer || !pub.VerifySignature(signBytes, wire.Signatures[0]) {
						t.Error("invalid SIGN_MODE_DIRECT signature or signer")
						w.WriteHeader(400)
						return
					}
					byHash[sha256Hex(raw)] = raw
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"code":0,"hash":"` + sha256Hex(raw) + `"}}`))
				case "tx":
					raw := byHash[strings.ToUpper(req.Params.Hash[2:])]
					if raw == nil {
						_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"error":{"message":"unknown hash"}}`))
						return
					}
					h := sha256Hex(raw)
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"hash":"` + h + `","height":"1","tx":"` + base64.StdEncoding.EncodeToString(raw) + `","tx_result":{"code":0}}}`))
				default:
					_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
				}
			}))
			defer server.Close()
			seedPath := filepath.Join(t.TempDir(), "seeds.json")
			payload, _ := json.Marshal(seedFile{ChainID: "fixture-1", Accounts: seeds})
			if err := os.WriteFile(seedPath, payload, 0600); err != nil {
				t.Fatal(err)
			}
			output := fixtureOutput(t)
			if err := run(context.Background(), server.URL, server.URL, "fixture-1", seedPath, "urp1", output, workload.duration, time.Second, time.Millisecond, 2, workload.ops, 0, 2, workload.envelopes); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			count := len(submitted)
			mu.Unlock()
			if workload.envelopes > 0 && (count != workload.envelopes) {
				t.Fatalf("finite workload submitted %d envelopes, expected %d", count, workload.envelopes)
			}
			if count == 0 {
				t.Fatal("fixture did not receive a signed envelope")
			}
			artifact, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var result result
			if err := json.Unmarshal(artifact, &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Attempts) != count || result.CommittedOperations != int64(workload.ops*count) || result.SuccessfulEnvelopes != int64(count) || result.RunError != "" {
				t.Fatalf("artifact totals do not match observed signed work: %s", artifact)
			}
			if result.RequestedEnvelopes != workload.envelopes {
				t.Fatalf("requested workload missing from artifact: %d", result.RequestedEnvelopes)
			}
			for _, attempt := range result.Attempts {
				raw, err := base64.StdEncoding.DecodeString(attempt.SignedTxBase64)
				if err != nil || sha256Hex(raw) != attempt.Hash || attempt.ObservedAt == nil || attempt.Outcome != "committed" {
					t.Fatalf("incomplete raw observation: %+v", attempt)
				}
			}
			t.Logf("signed fixture artifact: %s", output)
		})
	}
}

func configurePrefixes() {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("rp1", "rp1pub")
	cfg.Seal()
}
func sha256Hex(raw []byte) string {
	h := sha256.Sum256(raw)
	return strings.ToUpper(hex.EncodeToString(h[:]))
}

func TestRunFixtureFailureOutcomes(t *testing.T) {
	for _, mode := range []string{"execution_failed", "hash_mismatch", "bytes_mismatch", "zero_height", "unknown_admission", "unobserved"} {
		t.Run(mode, func(t *testing.T) {
			seeds := fixtureSeeds(t, 1)
			var broadcasts int
			var raw []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/cosmos/auth/") {
					_, _ = w.Write([]byte(`{"account":{"account_number":"1","sequence":"0"}}`))
					return
				}
				var req struct {
					Method string `json:"method"`
					Params struct {
						Tx string `json:"tx"`
					} `json:"params"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				switch req.Method {
				case "status":
					_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"fixture-1"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
				case "broadcast_tx_sync":
					broadcasts++
					raw, _ = base64.StdEncoding.DecodeString(req.Params.Tx)
					if mode == "unknown_admission" {
						w.WriteHeader(503)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"code": 0, "hash": sha256Hex(raw)}})
				case "tx":
					if mode == "unobserved" {
						_, _ = w.Write([]byte(`{"error":{"code":-32603,"message":"tx not found"}}`))
						return
					}
					hash := sha256Hex(raw)
					if mode == "hash_mismatch" {
						hash = strings.Repeat("0", 64)
					}
					code := 0
					if mode == "execution_failed" {
						code = 7
					}
					height, returnedBytes := "1", base64.StdEncoding.EncodeToString(raw)
					if mode == "bytes_mismatch" {
						returnedBytes = "YQ=="
					}
					if mode == "zero_height" {
						height = "0"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"hash": hash, "height": height, "tx": returnedBytes, "tx_result": map[string]any{"code": code}}})
				}
			}))
			defer server.Close()
			dir := t.TempDir()
			seedPath, output := filepath.Join(dir, "seeds.json"), fixtureOutput(t)
			payload, _ := json.Marshal(seedFile{ChainID: "fixture-1", Accounts: seeds})
			if err := os.WriteFile(seedPath, payload, 0600); err != nil {
				t.Fatal(err)
			}
			err := run(context.Background(), server.URL, server.URL, "fixture-1", seedPath, "urp1", output, 10*time.Millisecond, 25*time.Millisecond, time.Millisecond, 1, 2, 0, 2, 0)
			if err == nil {
				t.Fatal("failed benchmark must return an error")
			}
			if broadcasts != 1 {
				t.Fatalf("failed/ambiguous operation rebroadcast %d times", broadcasts)
			}
			artifact, err := os.ReadFile(output)
			if err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(artifact, &result); err != nil {
				t.Fatal(err)
			}
			if result["run_error"] == "" || result["run_error"] == nil {
				t.Fatal("partial artifact must preserve run_error")
			}
			attempts, ok := result["attempts"].([]any)
			if !ok || len(attempts) != 1 {
				t.Fatalf("raw signed attempt missing: %s", artifact)
			}
			t.Logf("failure artifact: %s", output)
		})
	}
}

func TestRunFiniteEnvelopeHonorsAdmissionWindow(t *testing.T) {
	seeds := fixtureSeeds(t, 1)
	var submitted atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/cosmos/auth/") {
			_, _ = w.Write([]byte(`{"account":{"account_number":"1","sequence":"0"}}`))
			return
		}
		var req struct {
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "status" {
			_, _ = w.Write([]byte(`{"result":{"node_info":{"network":"fixture-1"},"consensus_running":true,"sync_info":{"catching_up":false}}}`))
			return
		}
		if req.Method == "broadcast_tx_sync" {
			submitted.Add(1)
		}
		_, _ = w.Write([]byte(`{"result":{}}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	seedPath, output := filepath.Join(dir, "seeds.json"), filepath.Join(dir, "result.json")
	payload, _ := json.Marshal(seedFile{ChainID: "fixture-1", Accounts: seeds})
	if err := os.WriteFile(seedPath, payload, 0600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), server.URL, server.URL, "fixture-1", seedPath, "urp1", output, time.Nanosecond, time.Second, time.Millisecond, 1, 6137, 0, 2, 1)
	if err == nil {
		t.Fatal("finite workload past its admission window must fail")
	}
	if got := submitted.Load(); got != 0 {
		t.Fatalf("finite workload broadcast %d envelopes after deadline", got)
	}
}

func fixtureOutput(t *testing.T) string {
	t.Helper()
	dir := os.Getenv("RP1_BENCHMARK_E2E_ARTIFACT_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+".json")
}

func fixtureSeeds(t *testing.T, count int) []seedAccount {
	t.Helper()
	kb := keyring.NewInMemory(encodingConfig().Codec)
	seeds := make([]seedAccount, count)
	for i := range seeds {
		name := fmt.Sprintf("fixture-%d", i)
		path := "m/44'/118'/0'/0/0"
		record, mnemonic, err := kb.NewMnemonic(name, keyring.English, path, keyring.DefaultBIP39Passphrase, hd.Secp256k1)
		if err != nil {
			t.Fatal(err)
		}
		address, err := record.GetAddress()
		if err != nil {
			t.Fatal(err)
		}
		seeds[i] = seedAccount{Name: name, Mnemonic: mnemonic, Address: address.String(), HDPath: path}
	}
	return seeds
}
