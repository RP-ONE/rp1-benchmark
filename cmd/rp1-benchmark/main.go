// Command rp1-benchmark drives signed, batched bank transfers against an RP1
// JSON-RPC endpoint. It deliberately depends only on the public Cosmos SDK;
// the RP1 daemon and its private implementation are external dependencies.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/tx"
	"github.com/cosmos/cosmos-sdk/codec"
	"github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/tx/signing"
	authtx "github.com/cosmos/cosmos-sdk/x/auth/tx"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	banktypes "github.com/cosmos/cosmos-sdk/x/bank/types"
)

type seedFile struct {
	ChainID  string        `json:"chain_id"`
	Accounts []seedAccount `json:"accounts"`
}
type seedAccount struct {
	Name     string `json:"name"`
	Address  string `json:"address"`
	Mnemonic string `json:"mnemonic"`
	Algo     string `json:"algo"`
	HDPath   string `json:"hd_path"`
}
type account struct {
	name             string
	address          sdk.AccAddress
	number, sequence uint64
}
type rpcClient struct {
	endpoint string
	http     *http.Client
	id       atomic.Uint64
}
type rpcResponse struct {
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}
type accountResponse struct {
	Account struct {
		AccountNumber string `json:"account_number"`
		Sequence      string `json:"sequence"`
		BaseAccount   *struct {
			AccountNumber string `json:"account_number"`
			Sequence      string `json:"sequence"`
		} `json:"base_account,omitempty"`
	} `json:"account"`
}
type observation struct {
	Hash           string     `json:"hash"`
	SignedTxBase64 string     `json:"signed_tx_base64"`
	Signer         string     `json:"signer"`
	AccountNumber  uint64     `json:"account_number"`
	Sequence       uint64     `json:"sequence"`
	SubmittedAt    time.Time  `json:"submitted_at"`
	QueuedAt       *time.Time `json:"queued_at,omitempty"`
	ObservedAt     *time.Time `json:"observed_at,omitempty"`
	Height         int64      `json:"height,omitempty"`
	Code           uint32     `json:"code"`
	Outcome        string     `json:"outcome"`
	Error          string     `json:"error,omitempty"`
}

type rpcError struct {
	Code    int
	Message string
}

func (e *rpcError) Error() string { return e.Message }

func main() {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("rp1", "rp1pub")
	cfg.SetBech32PrefixForValidator("rp1valoper", "rp1valoperpub")
	cfg.SetBech32PrefixForConsensusNode("rp1valcons", "rp1valconspub")
	cfg.Seal()
	var rpc, rest, chainID, seeds, denom, output string
	var duration, finalityTimeout, poll time.Duration
	var concurrency, ops, envelopes int
	var gas, fee uint64
	flag.StringVar(&rpc, "rpc", "http://127.0.0.1:26657", "RP1 JSON-RPC endpoint")
	flag.StringVar(&rest, "rest", "", "Cosmos REST endpoint (defaults to rpc host port 1317)")
	flag.StringVar(&chainID, "chain-id", "", "chain ID used in SIGN_MODE_DIRECT")
	flag.StringVar(&seeds, "seed-accounts-file", "", "JSON file containing funded mnemonics")
	flag.StringVar(&denom, "denom", "urp1", "native transfer denom")
	flag.StringVar(&output, "output", "", "write JSON result to this file")
	flag.DurationVar(&duration, "duration", 20*time.Second, "admission window")
	flag.DurationVar(&finalityTimeout, "finality-timeout", 30*time.Second, "maximum wait per transaction")
	flag.DurationVar(&poll, "poll", 50*time.Millisecond, "committed transaction query cadence")
	flag.IntVar(&concurrency, "concurrency", 20, "simultaneous signed envelopes")
	flag.IntVar(&ops, "operations-per-envelope", 100, "number of MsgMultiSend outputs in each envelope")
	flag.IntVar(&envelopes, "envelopes", 0, "exact number of envelopes to submit; 0 uses the duration window")
	flag.Uint64Var(&gas, "gas", 0, "gas limit per envelope; 0 estimates 100000 + 15000 per output")
	flag.Uint64Var(&fee, "fee-urp1", 2, "native fee per signed envelope in urp1")
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := run(ctx, rpc, rest, chainID, seeds, denom, output, duration, finalityTimeout, poll, concurrency, ops, gas, fee, envelopes); err != nil {
		fmt.Fprintln(os.Stderr, "rp1-benchmark:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, rpcURL, restURL, chainID, seedPath, denom, output string, duration, finality, poll time.Duration, concurrency, operations int, gas, fee uint64, envelopes int) error {
	if chainID == "" || seedPath == "" || concurrency < 1 || concurrency > 4096 || operations < 1 || operations > 100000 || envelopes < 0 || duration <= 0 || finality <= 0 || poll <= 0 || fee > math.MaxInt64 {
		return errors.New("chain ID, seed file, positive duration/finality/poll, concurrency 1..4096, operations 1..100000, non-negative envelopes and an integer fee within int64 are required")
	}
	if denom != "urp1" {
		return errors.New("this native workload supports urp1 only")
	}
	if restURL == "" {
		restURL = defaultREST(rpcURL)
	}
	for _, endpoint := range []string{rpcURL, restURL} {
		u, err := url.Parse(endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("RPC and REST must be absolute HTTP(S) endpoints without credentials, queries, or fragments")
		}
	}
	if gas == 0 {
		gas = 100000 + uint64(operations)*15000
	}
	b, err := os.ReadFile(seedPath)
	if err != nil {
		return err
	}
	var seeds seedFile
	if err := json.Unmarshal(b, &seeds); err != nil {
		return fmt.Errorf("decode seed accounts: %w", err)
	}
	if seeds.ChainID != chainID || len(seeds.Accounts) < concurrency {
		return fmt.Errorf("seed file chain_id must equal %q and contain at least %d accounts", chainID, concurrency)
	}
	rpc := &rpcClient{endpoint: strings.TrimRight(rpcURL, "/"), http: &http.Client{Timeout: 15 * time.Second}}
	var status struct {
		NodeInfo struct {
			Network string `json:"network"`
		} `json:"node_info"`
		ConsensusRunning bool `json:"consensus_running"`
		SyncInfo         struct {
			CatchingUp bool `json:"catching_up"`
		} `json:"sync_info"`
	}
	if err := rpc.call(ctx, "status", map[string]any{}, &status); err != nil {
		return fmt.Errorf("network readiness: %w", err)
	}
	if status.NodeInfo.Network != chainID || !status.ConsensusRunning || status.SyncInfo.CatchingUp {
		return errors.New("RPC must report the configured chain ID with running consensus and no catch-up")
	}
	codecCfg := encodingConfig()
	kb := keyring.NewInMemory(codecCfg.Codec)
	accounts := make([]account, concurrency)
	seen := make(map[string]bool)
	for i := range accounts {
		seed := seeds.Accounts[i]
		if seed.Algo != "" && seed.Algo != "secp256k1" {
			return errors.New("only secp256k1 seed accounts are supported")
		}
		path := seed.HDPath
		if path == "" {
			path = "m/44'/118'/0'/0/0"
		}
		rec, err := kb.NewAccount(seed.Name, seed.Mnemonic, keyring.DefaultBIP39Passphrase, path, hd.Secp256k1)
		if err != nil {
			return fmt.Errorf("import %s: %w", seed.Name, err)
		}
		addr, err := rec.GetAddress()
		if err != nil {
			return err
		}
		declared, err := sdk.AccAddressFromBech32(seed.Address)
		if err != nil || !addr.Equals(declared) || seen[addr.String()] {
			return fmt.Errorf("seed %s must have a unique address matching its mnemonic", seed.Name)
		}
		seen[addr.String()] = true
		n, seq, err := queryAccount(ctx, restURL, addr)
		if err != nil {
			return fmt.Errorf("query %s: %w", seed.Name, err)
		}
		accounts[i] = account{name: seed.Name, address: addr, number: n, sequence: seq}
	}
	clientCtx := client.Context{}.WithCodec(codecCfg.Codec).WithTxConfig(codecCfg.TxConfig).WithChainID(chainID).WithKeyring(kb)
	started := time.Now()
	deadline := started.Add(duration)
	var wg sync.WaitGroup
	var mu sync.Mutex
	observations := make([]observation, 0)
	var runErr error
	var issued atomic.Int64
	add := func(ob observation, err error) {
		mu.Lock()
		defer mu.Unlock()
		observations = append(observations, ob)
		if err != nil && runErr == nil {
			runErr = err
		}
	}
	for i := range accounts {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			acc := accounts[i]
			for ctx.Err() == nil {
				if !time.Now().Before(deadline) {
					return
				}
				if envelopes > 0 {
					n := issued.Add(1)
					if n > int64(envelopes) {
						return
					}
				}
				raw, hash, err := buildTx(clientCtx, acc, accounts, acc.sequence, denom, operations, gas, fee)
				if err != nil {
					add(observation{Signer: acc.address.String(), Sequence: acc.sequence, Outcome: "signing_failed", Error: err.Error()}, err)
					return
				}
				if !time.Now().Before(deadline) {
					return
				}
				ob := observation{Hash: hash, SignedTxBase64: base64.StdEncoding.EncodeToString(raw), Signer: acc.address.String(), AccountNumber: acc.number, Sequence: acc.sequence, SubmittedAt: time.Now(), Outcome: "unknown_admission"}
				if err := rpc.broadcast(ctx, raw); err != nil {
					var rejection *rpcError
					if errors.As(err, &rejection) {
						ob.Outcome = "rejected"
					}
					ob.Error = err.Error()
					add(ob, fmt.Errorf("worker %d broadcast: %w", i, err))
					return
				}
				queued := time.Now()
				ob.QueuedAt = &queued
				waitCtx, cancel := context.WithTimeout(ctx, finality)
				code, height, err := rpc.waitTx(waitCtx, hash, raw, poll)
				cancel()
				if err != nil {
					ob.Outcome = "unobserved"
					ob.Error = err.Error()
					add(ob, fmt.Errorf("worker %d finality: %w", i, err))
					return
				}
				observed := time.Now()
				ob.ObservedAt = &observed
				ob.Height = height
				ob.Code = code
				ob.Outcome = "committed"
				if code != 0 {
					ob.Outcome = "execution_failed"
					ob.Error = fmt.Sprintf("committed execution code %d", code)
					add(ob, errors.New(ob.Error))
					return
				}
				add(ob, nil)
				acc.sequence++
			}
		}(i)
	}
	wg.Wait()
	if ctx.Err() != nil && runErr == nil {
		runErr = ctx.Err()
	}
	if len(observations) == 0 && runErr == nil {
		runErr = errors.New("no signed attempts completed; increase the duration")
	}
	if envelopes > 0 && runErr == nil && len(observations) != envelopes {
		runErr = fmt.Errorf("finite workload committed %d of %d requested envelopes", len(observations), envelopes)
	}
	finished := time.Now()
	sort.Slice(observations, func(i, j int) bool { return observations[i].SubmittedAt.Before(observations[j].SubmittedAt) })
	result := makeReport(chainID, rpcURL, started, finished, duration, poll, concurrency, operations, gas, fee, envelopes, observations, runErr)
	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if output != "" {
		if err := os.WriteFile(output, append(encoded, '\n'), 0600); err != nil {
			return err
		}
	}
	fmt.Println(string(encoded))
	return runErr
}

type result struct {
	ChainID                  string        `json:"chain_id"`
	RPC                      string        `json:"rpc"`
	Mode                     string        `json:"mode"`
	StartedAt                time.Time     `json:"started_at"`
	FinishedAt               time.Time     `json:"finished_at"`
	DurationSeconds          float64       `json:"duration_seconds"`
	AdmissionWindowSeconds   float64       `json:"admission_window_seconds"`
	PollSeconds              float64       `json:"poll_seconds"`
	Concurrency              int           `json:"concurrency"`
	OperationsPerEnvelope    int           `json:"operations_per_envelope"`
	Gas                      uint64        `json:"gas_limit"`
	Fee                      uint64        `json:"fee_urp1"`
	SignedEnvelopes          int64         `json:"signed_envelopes"`
	RequestedEnvelopes       int           `json:"requested_envelopes,omitempty"`
	QueuedEnvelopes          int64         `json:"queued_envelopes"`
	CommittedEnvelopes       int64         `json:"committed_envelopes"`
	SuccessfulEnvelopes      int64         `json:"successful_envelopes"`
	CommittedOperations      int64         `json:"committed_operations"`
	SignedEnvelopesPerSecond float64       `json:"signed_envelopes_per_second"`
	OperationsPerSecond      float64       `json:"operations_per_second"`
	FinalityP50              string        `json:"finality_p50"`
	FinalityP95              string        `json:"finality_p95"`
	FinalityP99              string        `json:"finality_p99"`
	FinalitySamples          int           `json:"finality_samples"`
	WorkersStopped           bool          `json:"workers_stopped"`
	RunError                 string        `json:"run_error,omitempty"`
	Attempts                 []observation `json:"attempts"`
	Note                     string        `json:"note"`
}

func makeReport(chain, rpc string, started, finished time.Time, window, poll time.Duration, c, ops int, gas, fee uint64, requested int, observations []observation, runErr error) result {
	r := result{ChainID: chain, RPC: rpc, Mode: "live", StartedAt: started, FinishedAt: finished, DurationSeconds: finished.Sub(started).Seconds(), AdmissionWindowSeconds: window.Seconds(), PollSeconds: poll.Seconds(), Concurrency: c, OperationsPerEnvelope: ops, Gas: gas, Fee: fee, RequestedEnvelopes: requested, WorkersStopped: true, Attempts: observations, Note: "Operations are successful MsgMultiSend outputs; envelope rate counts signed attempts, including failures. Rates cover the entire workflow including drain. This client has no validator-resource or cluster-convergence release gate and does not reproduce historical peak claims. Finality includes committed execution failures; fewer than 20 samples cannot support a finality claim."}
	if runErr != nil {
		r.RunError = runErr.Error()
	}
	latencies := make([]time.Duration, 0, len(observations))
	for _, ob := range observations {
		if ob.SignedTxBase64 != "" {
			r.SignedEnvelopes++
		}
		if ob.QueuedAt != nil {
			r.QueuedEnvelopes++
		}
		if ob.ObservedAt != nil {
			r.CommittedEnvelopes++
			latencies = append(latencies, ob.ObservedAt.Sub(ob.SubmittedAt))
			if ob.Code == 0 {
				r.SuccessfulEnvelopes++
				r.CommittedOperations += int64(ops)
			}
		}
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	percentile := func(p float64) string {
		if len(latencies) == 0 {
			return ""
		}
		return latencies[int(math.Ceil(p*float64(len(latencies))))-1].String()
	}
	r.FinalitySamples = len(latencies)
	r.FinalityP50, r.FinalityP95, r.FinalityP99 = percentile(.5), percentile(.95), percentile(.99)
	if r.DurationSeconds > 0 {
		r.SignedEnvelopesPerSecond = float64(r.SignedEnvelopes) / r.DurationSeconds
		r.OperationsPerSecond = float64(r.CommittedOperations) / r.DurationSeconds
	}
	return r
}

type encodingConfigValue struct {
	Codec    codec.Codec
	TxConfig client.TxConfig
}

func encodingConfig() encodingConfigValue {
	r := types.NewInterfaceRegistry()
	banktypes.RegisterInterfaces(r)
	authtypes.RegisterInterfaces(r)
	cryptocodec.RegisterInterfaces(r)
	c := codec.NewProtoCodec(r)
	return encodingConfigValue{Codec: c, TxConfig: authtx.NewTxConfig(c, []signing.SignMode{signing.SignMode_SIGN_MODE_DIRECT})}
}
func buildTx(ctx client.Context, from account, all []account, sequence uint64, denom string, operations int, gas, fee uint64) ([]byte, string, error) {
	outs := make([]banktypes.Output, operations)
	for i := range outs {
		to := all[(i+1)%len(all)].address
		outs[i] = banktypes.Output{Address: to.String(), Coins: sdk.NewCoins(sdk.NewInt64Coin(denom, 1))}
	}
	msg := &banktypes.MsgMultiSend{Inputs: []banktypes.Input{{Address: from.address.String(), Coins: sdk.NewCoins(sdk.NewInt64Coin(denom, int64(operations)))}}, Outputs: outs}
	f := tx.Factory{}.WithChainID(ctx.ChainID).WithTxConfig(ctx.TxConfig).WithKeybase(ctx.Keyring).WithFromName(from.name).WithAccountNumber(from.number).WithSequence(sequence).WithGas(gas).WithFees(fmt.Sprintf("%d%s", fee, denom))
	b, err := f.BuildUnsignedTx(msg)
	if err != nil {
		return nil, "", err
	}
	if err = tx.Sign(context.Background(), f, from.name, b, true); err != nil {
		return nil, "", err
	}
	raw, err := ctx.TxConfig.TxEncoder()(b.GetTx())
	if err != nil {
		return nil, "", err
	}
	h := sha256.Sum256(raw)
	return raw, strings.ToUpper(hex.EncodeToString(h[:])), nil
}

func (r *rpcClient) broadcast(ctx context.Context, raw []byte) error {
	var v struct {
		Code uint32 `json:"code"`
		Log  string `json:"log"`
		Hash string `json:"hash"`
	}
	if err := r.call(ctx, "broadcast_tx_sync", map[string]string{"tx": base64.StdEncoding.EncodeToString(raw)}, &v); err != nil {
		return err
	}
	hash := sha256.Sum256(raw)
	if !strings.EqualFold(v.Hash, hex.EncodeToString(hash[:])) {
		return errors.New("broadcast returned a mismatched transaction hash; admission is unknown")
	}
	if v.Code != 0 {
		return &rpcError{Code: int(v.Code), Message: fmt.Sprintf("broadcast code %d: %s", v.Code, v.Log)}
	}
	return nil
}
func (r *rpcClient) waitTx(ctx context.Context, hash string, expected []byte, poll time.Duration) (uint32, int64, error) {
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		var v struct {
			Hash     string `json:"hash"`
			Height   string `json:"height"`
			Tx       string `json:"tx"`
			TxResult *struct {
				Code uint32 `json:"code"`
			} `json:"tx_result"`
		}
		err := r.call(ctx, "tx", map[string]string{"hash": "0x" + hash}, &v)
		if err == nil {
			h, parseErr := strconv.ParseInt(v.Height, 10, 64)
			decoded, decodeErr := base64.StdEncoding.DecodeString(v.Tx)
			if parseErr != nil || h <= 0 || !strings.EqualFold(v.Hash, hash) || decodeErr != nil || !bytes.Equal(decoded, expected) || v.TxResult == nil {
				return 0, 0, errors.New("transaction observation did not match submitted hash, bytes, execution result, or positive height")
			}
			return v.TxResult.Code, h, nil
		}
		var missing *rpcError
		if !errors.As(err, &missing) || missing.Message != "tx not found" {
			return 0, 0, err
		}
		select {
		case <-ctx.Done():
			return 0, 0, ctx.Err()
		case <-t.C:
		}
	}
}
func (r *rpcClient) call(ctx context.Context, method string, params any, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": r.id.Add(1), "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("rpc HTTP %s", resp.Status)
	}
	var envelope rpcResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&envelope); err != nil {
		return err
	}
	if envelope.Error != nil {
		return &rpcError{Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	if len(envelope.Result) == 0 || string(envelope.Result) == "null" {
		return errors.New("RPC returned no result")
	}
	return json.Unmarshal(envelope.Result, out)
}

func queryAccount(ctx context.Context, base string, addr sdk.AccAddress) (uint64, uint64, error) {
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, strings.TrimRight(base, "/")+"/cosmos/auth/v1beta1/accounts/"+addr.String(), nil)
	if err != nil {
		return 0, 0, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return 0, 0, fmt.Errorf("REST HTTP %s", resp.Status)
	}
	var v accountResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&v); err != nil {
		return 0, 0, err
	}
	number, err := strconv.ParseUint(v.Account.AccountNumber, 10, 64)
	if err != nil && v.Account.BaseAccount != nil {
		number, err = strconv.ParseUint(v.Account.BaseAccount.AccountNumber, 10, 64)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("invalid account number: %w", err)
	}
	sequence, err := strconv.ParseUint(v.Account.Sequence, 10, 64)
	if err != nil && v.Account.BaseAccount != nil {
		sequence, err = strconv.ParseUint(v.Account.BaseAccount.Sequence, 10, 64)
	}
	if err != nil {
		return 0, 0, fmt.Errorf("invalid sequence: %w", err)
	}
	return number, sequence, nil
}
func defaultREST(rpc string) string {
	s := strings.TrimRight(rpc, "/")
	s = strings.Replace(s, ":26657", ":1317", 1)
	return s
}
