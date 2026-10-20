# RP1 benchmark client

Run signed, batched RP1 benchmarks with an automatically provisioned private validator network, or against the shared testnet. The setup downloads the official validator binary; private chain source is never required or distributed.

## Private benchmark: one command

Use a **native Linux x86-64 host**, Python **3.10+**, Go **1.24+**, and Git. For a four-validator evaluation, provision a host with at least 8 CPU cores, 16 GiB RAM, SSD storage, and 20 GiB free disk. This is a starting configuration, not a performance guarantee. For capacity measurements, place the load generator on a separate host using the manual client command below. macOS/ARM can run the client against an existing network; the published validator is Linux x86-64 only.

```sh
git clone https://github.com/RP-ONE/rp1-benchmark.git
cd rp1-benchmark
python3 scripts/benchmark.py private
```

That command:

1. Downloads `rp1d` **0.25.4** and the public licence, notices, provenance, checksums, and signature bundle.
2. Checks the pinned signed-manifest digest, all asset checksums, archive layout, and provenance before making the binary executable.
3. Builds the benchmark client and starts **four validators**, using 20 ms batching and 50 ms consensus rounds.
4. Generates **20 fresh accounts funded in genesis**. Keys stay in a private local directory; no shared funded wallet or faucet key is published.
5. Waits for distinct validator identities, running consensus, positive heights, and matching retained block height/hash/app hash/time.
6. Runs a 20-second benchmark with 20 concurrent envelopes, 100 operations per envelope, and 5 ms transaction polling, then stops the validators.

The local network uses `0urp1` minimum gas prices and the default run pays zero fees. This selected configuration is recorded evidence for that run, not a public-network fee policy. REST/gRPC/EVM listeners bind to loopback. The published daemon's managed setup requires wildcard RPC listeners; use a private host/network or firewall those ports. The client connects through loopback.

Outputs are in `.rp1-benchmark/private/`:

| File | Contents |
| --- | --- |
| `result.json` | Signed submissions, committed observations, timings, rates, and failures |
| `setup.json` | Configuration, platform, validator identities, convergence headers, release digest, and setup errors |
| `validators.log` | Node setup and shutdown output |
| `network/rp1-benchmark-1/seed-accounts/accounts.json` | Locally generated test keys; keep private |
| `validator/release-assets/` | Verified distribution assets, licence, and notices |

Repeat the same command to reuse the persisted chain and accounts. A concurrent run is refused. Changing the validator/account count requires a new state directory, preserving the previous network:

```sh
python3 scripts/benchmark.py private \
  --state-dir "$HOME/rp1-benchmark-larger" \
  --validators 4 --concurrency 40 \
  --operations-per-envelope 1000 --duration 60s
```

`Ctrl-C` stops the benchmark and validator process group. State is retained for inspection and restart. Setup failures exit nonzero and write a partial `setup.json`; transaction failures retain a partial `result.json`. A downloaded asset that fails verification is never executed. The installer pins the checksum manifest whose Sigstore signature was independently verified; the complete bundle is retained for independent verification using the [release verification instructions](https://github.com/RP-ONE/rp1-releases#install-and-verify).

## Shared testnet

```sh
python3 scripts/benchmark.py testnet
```

This discovers the official RPC/REST endpoints from the [public bootstrap manifest](https://testnet.chain.rp.one/bootstrap.json), checks network readiness, creates fresh local accounts, requests tokens from the hosted faucet, and confirms actual native balances before starting the benchmark. Existing funded accounts are reused. Faucet failures and rate limits stop the run without automatic claim retries. Submitted claims, including pending responses, must reach the required on-chain balance before setup continues. Funding audits contain addresses and outcomes, never mnemonics. The default fee is calculated from the node's published minimum gas price; `--fee-urp1` selects an explicit amount.

This command benchmarks existing active testnet validators. To synchronize a fresh local observer with the running chain first, use `joined-testnet` below. Public RPC/REST and fresh-account faucet funding were verified on 20 October 2026. Service failures stop setup before benchmark traffic starts.

## Joined testnet observer

Synchronize a fresh observer and run the shared-chain benchmark with:

```sh
python3 scripts/benchmark.py joined-testnet
```

This mode downloads and verifies the official validator release, refuses releases that do not expose `testnet join --state-sync`, creates a fresh private observer home, and joins from the HTTPS public bootstrap manifest. It waits for the observer and public RPC to agree on block hash, application hash, and block time at a common retained height, stops and restarts the observer, and requires continued height progress before requesting fresh accounts from the faucet. Signed benchmark traffic is sent through the active public RPC/REST endpoints because an observer is read-only; each committed hash is then checked through the observer's REST history. The observer remains a non-voting node; no validator admission transaction or shared private key is used.

For existing validator operators, the [validator join guide](https://docs.rp.one/operate/join-validator/) documents the bootstrap and admission commands. Running an isolated private network and participating in the shared chain are separate configurations: a private run has its own genesis, funded accounts, and validators.

## Module testing links and CLI

Use the [wallet module page](https://app.rp.one/modules), [testnet faucet](https://app.rp.one/faucet), or [MCP endpoint](https://mcp.rp.one/mcp) for treasury approvals, institutional mint/redeem and MPP charges/refunds. The wallet reviews each action before local signing and shows the committed hash, height and created entity IDs. If an outcome is unknown, reconcile the existing hash before submitting another action.

The same official validator binary includes native query and transaction commands:

```sh
rp1d query enterprise --help
rp1d tx enterprise --help
rp1d query stables --help
rp1d tx stables --help
rp1d query mpp --help
rp1d tx mpp --help
```

Use `--node https://testnet.rpc.rp.one --chain-id chain-1` with your own local keyring. Treasury payments enforce the configured approvers and threshold. Institutional minting requires a governance-approved denom, its authorized issuer and matching native reserves; holders can redeem issued tokens. MPP payment commands bind the expected amount, denom and recipient before settlement. Public test assets have no monetary value.

## Client workload

The client signs standard Cosmos `bank.MsgMultiSend` transactions with `SIGN_MODE_DIRECT`, broadcasts them through RP1 JSON-RPC, and observes each transaction until it is committed. Every envelope contains several outputs, so the report keeps the two units explicit:

* `signed_envelopes_per_second` counts signed transactions.
* `operations_per_second` counts successful `MsgMultiSend` outputs (`operations_per_envelope` times committed envelopes).

The workload keeps one envelope in flight per worker. Increase `--concurrency` for a multi-in-flight run; a one-in-flight run is useful for end-to-end latency and workflow rate, but does not establish saturation capacity.

## Run the client separately

Use Go 1.24 or newer and point the client at running RP1 RPC and REST endpoints. The [official validator release](https://github.com/RP-ONE/rp1-releases/releases/tag/0.25.4) supplies `rp1d` without private-source access. The automated private setup above installs it and creates the funded seed file for you.

Create a seed file with funded accounts. Keep it outside the repository and mode `0600`:

```json
{
  "chain_id": "chain-1",
  "accounts": [
    {
      "name": "bench-1",
      "address": "rp1...",
      "mnemonic": "twelve or twenty four words ...",
      "hd_path": "m/44'/118'/0'/0/0"
    }
  ]
}
```

There must be at least one funded account per worker. Do not commit this file or any mnemonic.

```sh
go run ./cmd/rp1-benchmark \
  --rpc http://127.0.0.1:26657 \
  --rest http://127.0.0.1:1317 \
  --chain-id chain-1 \
  --seed-accounts-file ./seed-accounts.json \
  --duration 20s \
  --concurrency 20 \
  --operations-per-envelope 100 \
  --gas 1600000 \
  --fee-urp1 10 \
  --poll 5ms \
  --output result.json
```

The client first checks the RPC chain ID, running consensus, and catch-up status. The REST endpoint reads account number and sequence. `--gas 0` estimates 100,000 plus 15,000 per output; set gas and fee for the actual deployment and batch size. A chosen fee proves only that run, not a sustainable fee minimum. The JSON-RPC endpoint handles broadcasts and committed transaction queries. The JSON includes every signed attempt, SHA-256 hash, signer/account/sequence, outcome, error, and submission/queue/observation timestamps. Counts are derived from those attempts. Both rates use the full elapsed workflow, including finality drain. Only code-zero committed outputs count as operations; finality percentiles include committed execution failures. An uncertain broadcast is never automatically resubmitted. Any broadcast, observation, or execution failure exits nonzero after writing a partial artifact with `run_error`. Fewer than 20 committed observations cannot support a finality claim.

## Interpreting results

Choose a rig that can sign and submit the configured number of envelopes without becoming the bottleneck. Use multiple funded accounts and meaningful `MsgMultiSend` batches; sending one operation in one envelope measures envelope latency, not the chain's batched operation throughput. Record CPU model and core count, memory, storage, node/Go versions, chain revision, validator count and configuration, network latency, batch size, worker count, and query cadence with every run. Prefer a dedicated load generator so signing and polling do not compete with validators. The public testnet is operated for functional integration and may be scaled down or unavailable; it is not a replica of an earlier multi-validator benchmark deployment.

This tool reports only the run it performs. It does not reproduce or certify historical RP1 dashboard figures such as a 613,700 operations/second database-second peak or a sub-350 ms finality result. Those figures came from separately scoped runs and require their original deployment, workload, and raw evidence. The historical peak run had 100 signed envelopes with 6,137 outputs each: 613,700 operations over about 11 seconds. Its dashboard peak used a database-second bucket and is not its whole-run average. This repository does not contain the indexed transaction buckets needed to independently reconstruct that historical peak. It also does not include the private validator-resource and cluster-convergence release gates; do not report its output as a release-gate pass.

## Verify the client

```sh
RP1_BENCHMARK_E2E_ARTIFACT_DIR=/tmp/rp1-benchmark-e2e go test -race -v ./...
python3 -m unittest discover -s tests -p 'test_*.py'
go vet ./...
go build ./cmd/rp1-benchmark
```

The E2E fixture uses two test accounts, verifies real `SIGN_MODE_DIRECT` signatures and `MsgMultiSend` output/fee accounting, then checks the persisted report against the received work. Failure scenarios cover execution failure, hash/bytes/height mismatch, uncertain broadcast, and unobserved commitment. Artifacts are written to the specified directory. These simulated endpoint checks verify the client; they are not chain TPS or finality evidence.

## License

Copyright 2026 RP-ONE. Licensed under Apache-2.0. See [LICENSE](LICENSE).
