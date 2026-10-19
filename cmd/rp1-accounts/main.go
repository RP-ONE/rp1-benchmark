// Command rp1-accounts creates disposable benchmark keys and obtains faucet
// funding without exposing private material to logs or audit artifacts.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/hd"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

const (
	defaultRPC    = "https://testnet.rpc.rp.one"
	defaultREST   = "https://testnet.rest.rp.one"
	defaultFaucet = "https://app.rp.one/api/faucet"
	defaultDenom  = "urp1"
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

type fundingAccount struct {
	Address string `json:"address"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

type fundingAudit struct {
	ChainID            string           `json:"chain_id"`
	Denom              string           `json:"denom"`
	MinimumBalanceUrp1 uint64           `json:"minimum_balance_urp1"`
	Accounts           []fundingAccount `json:"accounts"`
	Error              string           `json:"error,omitempty"`
}

type fundConfig struct {
	seedPath  string
	rpcURL    string
	restURL   string
	faucetURL string
	minimum   uint64
	timeout   time.Duration
	output    string
}

func main() {
	configurePrefixes()
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "create":
		err = runCreate(os.Args[2:])
	case "fund":
		err = runFund(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rp1-accounts:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: rp1-accounts create|fund [flags]")
}

func runCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	chainID := fs.String("chain-id", "", "chain ID recorded in the seed file")
	count := fs.Int("count", 1, "number of accounts (1..4096)")
	output := fs.String("output", "seed-accounts.json", "seed file path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return createAccounts(*chainID, *count, *output)
}

func runFund(args []string) error {
	fs := flag.NewFlagSet("fund", flag.ContinueOnError)
	var cfg fundConfig
	fs.StringVar(&cfg.seedPath, "seed-accounts-file", "", "seed file created by rp1-accounts create")
	fs.StringVar(&cfg.rpcURL, "rpc", defaultRPC, "RP1 JSON-RPC endpoint")
	fs.StringVar(&cfg.restURL, "rest", defaultREST, "RP1 REST endpoint")
	fs.StringVar(&cfg.faucetURL, "faucet-url", defaultFaucet, "faucet endpoint")
	fs.Uint64Var(&cfg.minimum, "minimum-balance-urp1", 1_000_000, "minimum native balance per account")
	fs.DurationVar(&cfg.timeout, "timeout", 120*time.Second, "overall funding timeout")
	fs.StringVar(&cfg.output, "output", "funding.json", "audit output path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fundAccounts(context.Background(), cfg)
}

func configurePrefixes() {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("rp1", "rp1pub")
	cfg.SetBech32PrefixForValidator("rp1valoper", "rp1valoperpub")
	cfg.SetBech32PrefixForConsensusNode("rp1valcons", "rp1valconspub")
	cfg.Seal()
}

func createAccounts(chainID string, count int, output string) error {
	if strings.TrimSpace(chainID) == "" {
		return errors.New("chain ID is required")
	}
	if count < 1 || count > 4096 {
		return errors.New("count must be 1..4096")
	}
	if err := ensurePrivateParent(output); err != nil {
		return err
	}
	if _, err := os.Stat(output); err == nil {
		return fmt.Errorf("refusing to overwrite %s", output)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	kb := keyring.NewInMemory(encodingConfig().Codec)
	seeds := seedFile{ChainID: chainID, Accounts: make([]seedAccount, 0, count)}
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("bench-%02d", i+1)
		path := fmt.Sprintf("m/44'/118'/0'/0/%d", i)
		record, mnemonic, err := kb.NewMnemonic(name, keyring.English, path, keyring.DefaultBIP39Passphrase, hd.Secp256k1)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		address, err := record.GetAddress()
		if err != nil {
			return fmt.Errorf("derive %s: %w", name, err)
		}
		seeds.Accounts = append(seeds.Accounts, seedAccount{Name: name, Address: address.String(), Mnemonic: mnemonic, Algo: "secp256k1", HDPath: path})
	}
	data, err := json.MarshalIndent(seeds, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create seed file: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write seed file: %w", err)
	}
	return nil
}

func ensurePrivateParent(path string) error {
	parent := filepath.Dir(path)
	if parent == "." {
		return nil
	}
	if _, err := os.Stat(parent); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	return os.Chmod(parent, 0o700)
}

func fundAccounts(ctx context.Context, cfg fundConfig) (err error) {
	if cfg.seedPath == "" || cfg.output == "" || cfg.timeout <= 0 || cfg.minimum == 0 {
		return errors.New("seed-accounts-file, output, positive timeout, and minimum balance are required")
	}
	for name, endpoint := range map[string]string{"RPC": cfg.rpcURL, "REST": cfg.restURL, "faucet": cfg.faucetURL} {
		if err := validateEndpoint(endpoint); err != nil {
			return fmt.Errorf("%s endpoint: %w", name, err)
		}
	}
	if err := rejectAuditAlias(cfg.seedPath, cfg.output); err != nil {
		return err
	}
	data, err := os.ReadFile(cfg.seedPath)
	if err != nil {
		return err
	}
	var seeds seedFile
	if err := json.Unmarshal(data, &seeds); err != nil {
		return fmt.Errorf("decode seed accounts: %w", err)
	}
	if seeds.ChainID == "" || len(seeds.Accounts) == 0 {
		return errors.New("seed file must contain a chain_id and at least one account")
	}
	audit := fundingAudit{ChainID: seeds.ChainID, Denom: defaultDenom, MinimumBalanceUrp1: cfg.minimum, Accounts: make([]fundingAccount, len(seeds.Accounts))}
	for i, seed := range seeds.Accounts {
		audit.Accounts[i] = fundingAccount{Address: seed.Address, Status: "pending"}
	}
	defer func() {
		if err != nil {
			audit.Error = err.Error()
		}
		if writeErr := writeAudit(cfg.output, audit); err == nil && writeErr != nil {
			err = writeErr
		}
	}()
	kb := keyring.NewInMemory(encodingConfig().Codec)
	seen := make(map[string]struct{}, len(seeds.Accounts))
	addresses := make([]string, len(seeds.Accounts))
	for i, seed := range seeds.Accounts {
		if seed.Name == "" || seed.Mnemonic == "" || seed.Address == "" {
			return errors.New("every seed account needs name, address, and mnemonic")
		}
		if seed.Algo != "" && seed.Algo != "secp256k1" {
			return errors.New("only secp256k1 seed accounts are supported")
		}
		path := seed.HDPath
		if path == "" {
			path = fmt.Sprintf("m/44'/118'/0'/0/%d", i)
		}
		record, err := kb.NewAccount(seed.Name, seed.Mnemonic, keyring.DefaultBIP39Passphrase, path, hd.Secp256k1)
		if err != nil {
			return fmt.Errorf("import %s: %w", seed.Name, err)
		}
		address, err := record.GetAddress()
		if err != nil {
			return err
		}
		declared, err := sdk.AccAddressFromBech32(seed.Address)
		if err != nil || !address.Equals(declared) {
			return fmt.Errorf("seed %s address does not match its mnemonic", seed.Name)
		}
		if _, ok := seen[address.String()]; ok {
			return errors.New("seed addresses must be unique")
		}
		seen[address.String()] = struct{}{}
		addresses[i] = address.String()
	}
	requestCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()
	status, err := rpcStatus(requestCtx, cfg.rpcURL)
	if err != nil {
		return fmt.Errorf("network readiness: %w", err)
	}
	if status.Network != seeds.ChainID || !status.ConsensusRunning || status.CatchingUp {
		return errors.New("RPC must report the seed chain ID with running consensus and no catch-up")
	}
	balance := make([]uint64, len(addresses))
	needsFunding := false
	for i, address := range addresses {
		balance[i], err = queryBalance(requestCtx, cfg.restURL, address)
		if err != nil {
			return fmt.Errorf("query %s balance: %w", address, err)
		}
		if balance[i] >= cfg.minimum {
			audit.Accounts[i].Status = "already_funded"
		} else {
			needsFunding = true
		}
	}
	if !needsFunding {
		return nil
	}
	meta, err := faucetMetadata(requestCtx, cfg.faucetURL)
	if err != nil {
		return fmt.Errorf("faucet preflight: %w", err)
	}
	if !meta.Enabled || meta.ChainID != seeds.ChainID || meta.Denom != defaultDenom || uint64(meta.Amount) < cfg.minimum {
		return errors.New("faucet metadata does not match the requested chain, denom, or minimum")
	}
	for i, address := range addresses {
		if audit.Accounts[i].Status == "already_funded" {
			continue
		}
		if err := claimAndWait(requestCtx, cfg, address); err != nil {
			audit.Accounts[i].Status = "failed"
			audit.Accounts[i].Error = err.Error()
			audit.Error = err.Error()
			return err
		}
		audit.Accounts[i].Status = "funded"
	}
	return nil
}

type networkStatus struct {
	Network          string
	ConsensusRunning bool
	CatchingUp       bool
}

func rpcStatus(ctx context.Context, endpoint string) (networkStatus, error) {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "status", "params": map[string]any{}})
	if err != nil {
		return networkStatus{}, err
	}
	response, err := httpJSON(ctx, accountHTTPClient, http.MethodPost, strings.TrimRight(endpoint, "/"), body)
	if err != nil {
		return networkStatus{}, err
	}
	var envelope struct {
		Result struct {
			NodeInfo struct {
				Network string `json:"network"`
			} `json:"node_info"`
			ConsensusRunning bool `json:"consensus_running"`
			SyncInfo         struct {
				CatchingUp bool `json:"catching_up"`
			} `json:"sync_info"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response, &envelope); err != nil {
		return networkStatus{}, err
	}
	return networkStatus{Network: envelope.Result.NodeInfo.Network, ConsensusRunning: envelope.Result.ConsensusRunning, CatchingUp: envelope.Result.SyncInfo.CatchingUp}, nil
}

type faucetInfo struct {
	Enabled bool         `json:"enabled"`
	ChainID string       `json:"chainId"`
	Denom   string       `json:"denom"`
	Amount  faucetAmount `json:"amount"`
}

type faucetAmount uint64

func (amount *faucetAmount) UnmarshalJSON(data []byte) error {
	value := strings.Trim(string(data), `"`)
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid faucet amount: %w", err)
	}
	*amount = faucetAmount(parsed)
	return nil
}

func faucetMetadata(ctx context.Context, endpoint string) (faucetInfo, error) {
	body, err := httpJSON(ctx, faucetHTTPClient, http.MethodGet, endpoint, nil)
	if err != nil {
		return faucetInfo{}, err
	}
	var info faucetInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return faucetInfo{}, err
	}
	return info, nil
}

func queryBalance(ctx context.Context, endpoint, address string) (uint64, error) {
	url := strings.TrimRight(endpoint, "/") + "/cosmos/bank/v1beta1/balances/" + url.PathEscape(address) + "/by_denom?denom=" + url.QueryEscape(defaultDenom)
	body, err := httpJSON(ctx, accountHTTPClient, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	var response struct {
		Balance struct {
			Denom  string `json:"denom"`
			Amount string `json:"amount"`
		} `json:"balance"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return 0, err
	}
	if response.Balance.Denom != defaultDenom {
		return 0, errors.New("balance response has unexpected denom")
	}
	return strconv.ParseUint(response.Balance.Amount, 10, 64)
}

func claimAndWait(ctx context.Context, cfg fundConfig, address string) error {
	body, err := json.Marshal(map[string]string{"address": address})
	if err != nil {
		return err
	}
	response, err := httpJSON(ctx, faucetHTTPClient, http.MethodPost, cfg.faucetURL, body)
	if err != nil {
		return fmt.Errorf("faucet claim: %w", err)
	}
	var claim struct {
		Success bool   `json:"success"`
		TxHash  string `json:"tx_hash"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal(response, &claim); err != nil {
		return err
	}
	if !claim.Success || claim.TxHash == "" {
		if claim.Error == "" {
			claim.Error = "faucet rejected claim"
		}
		return errors.New(claim.Error)
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		balance, err := queryBalance(ctx, cfg.restURL, address)
		if err == nil && balance >= cfg.minimum {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return fmt.Errorf("wait for faucet balance: %w", err)
			}
			return fmt.Errorf("wait for faucet balance: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

var (
	accountHTTPClient = &http.Client{Timeout: 15 * time.Second}
	faucetHTTPClient  = newFaucetHTTPClient()
)

func newFaucetHTTPClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// The public faucet can stall Go HTTP/2 connections before response headers.
	transport.Protocols = new(http.Protocols)
	transport.Protocols.SetHTTP1(true)
	return &http.Client{Timeout: 15 * time.Second, Transport: transport}
}

func httpJSON(ctx context.Context, client *http.Client, method, endpoint string, body []byte) ([]byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if readErr != nil {
		return nil, readErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(data)))
	}
	return data, nil
}

func writeAudit(path string, audit fundingAudit) error {
	data, err := json.MarshalIndent(audit, "", "  ")
	if err != nil {
		return err
	}
	if err := ensurePrivateParent(path); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return errors.New("refusing symlink audit output")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".rp1-funding-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer func() {
		_ = os.Remove(tempPath)
	}()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func rejectAuditAlias(seedPath, outputPath string) error {
	seedInfo, err := os.Stat(seedPath)
	if err != nil {
		return err
	}
	outputInfo, err := os.Lstat(outputPath)
	if err == nil {
		if outputInfo.Mode()&os.ModeSymlink != 0 {
			return errors.New("refusing symlink audit output")
		}
		resolvedOutput, statErr := os.Stat(outputPath)
		if statErr != nil {
			return statErr
		}
		if os.SameFile(seedInfo, resolvedOutput) {
			return errors.New("audit output must not alias the seed file")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func validateEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must be an absolute HTTP(S) URL without credentials, query, or fragment")
	}
	return nil
}

func encodingConfig() struct{ Codec codec.Codec } {
	interfaceRegistry := codectypes.NewInterfaceRegistry()
	return struct{ Codec codec.Codec }{Codec: codec.NewProtoCodec(interfaceRegistry)}
}
