# RP1 benchmark client

This repository is a small, standalone client for measuring signed, batched RP1 transactions. It contains no RP1 node, consensus, application, or private chain source. The daemon and a funded set of test accounts are external prerequisites.

The client signs standard Cosmos `bank.MsgMultiSend` transactions with `SIGN_MODE_DIRECT`, broadcasts them through RP1 JSON-RPC, and observes each transaction until it is committed. Every envelope contains several outputs, so the report keeps the two units explicit:

* `signed_envelopes_per_second` counts signed transactions.
* `operations_per_second` counts successful `MsgMultiSend` outputs (`operations_per_envelope` times committed envelopes).

The workload keeps one envelope in flight per worker. Increase `--concurrency` for a multi-in-flight run; a one-in-flight run is useful for end-to-end latency and workflow rate, but does not establish saturation capacity.

## Run

Use Go 1.24 or newer, `git clone https://github.com/RP-ONE/rp1-benchmark.git`, then `cd rp1-benchmark`. Point the client at a running RP1 RPC and REST endpoint. A self-hosted validator rig also needs an approved operator-provided `rp1d` binary: the [public release repository](https://github.com/RP-ONE/rp1-releases/releases) currently publishes a signer, not a daemon. This client does not provide node software or require private-source access. The benchmark client itself never needs the private node repository.

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
  --gas 600000 \
  --fee-urp1 2 \
  --poll 5ms \
  --output result.json
```

The client first checks the RPC chain ID, running consensus, and catch-up status. The REST endpoint reads account number and sequence. `--gas 0` estimates 100,000 plus 5,000 per output; set gas and fee for the actual deployment and batch size. A chosen fee proves only that run, not a sustainable fee minimum. The JSON-RPC endpoint handles broadcasts and committed transaction queries. The JSON includes every signed attempt, SHA-256 hash, signer/account/sequence, outcome, error, and submission/queue/observation timestamps. Counts are derived from those attempts. Both rates use the full elapsed workflow, including finality drain. Only code-zero committed outputs count as operations; finality percentiles include committed execution failures. An uncertain broadcast is never automatically resubmitted. Any broadcast, observation, or execution failure exits nonzero after writing a partial artifact with `run_error`. Fewer than 20 committed observations cannot support a finality claim.

## Interpreting results

Choose a rig that can sign and submit the configured number of envelopes without becoming the bottleneck. Use multiple funded accounts and meaningful `MsgMultiSend` batches; sending one operation in one envelope measures envelope latency, not the chain's batched operation throughput. Record CPU model and core count, memory, storage, node/Go versions, chain revision, validator count and configuration, network latency, batch size, worker count, and query cadence with every run. Prefer a dedicated load generator so signing and polling do not compete with validators. Agree on this rig and workload with RP1 before a diligence rerun. The public testnet is operated for functional integration and may be scaled down or unavailable; it is not a replica of an earlier multi-validator benchmark deployment.

This tool reports only the run it performs. It does not reproduce or certify historical RP1 dashboard figures such as a 613,700 operations/second database-second peak or a sub-350 ms finality result. Those figures came from separately scoped runs and require their original deployment, workload, and raw evidence. The historical peak run had 100 signed envelopes with 6,137 outputs each: 613,700 operations over about 11 seconds. Its dashboard peak used a database-second bucket and is not its whole-run average. This repository does not contain the indexed transaction buckets needed to independently reconstruct that historical peak. It also does not include the private validator-resource and cluster-convergence release gates; do not report its output as a release-gate pass.

## Verify the client

```sh
RP1_BENCHMARK_E2E_ARTIFACT_DIR=/tmp/rp1-benchmark-e2e go test -race -v ./...
go vet ./...
go build ./cmd/rp1-benchmark
```

The E2E fixture uses two test accounts, verifies real `SIGN_MODE_DIRECT` signatures and `MsgMultiSend` output/fee accounting, then checks the persisted report against the received work. Failure scenarios cover execution failure, hash/bytes/height mismatch, uncertain broadcast, and unobserved commitment. Artifacts are written to the specified directory. These simulated endpoint checks verify the client; they are not chain TPS or finality evidence.

## License

Copyright 2026 RP-ONE. Licensed under Apache-2.0. See [LICENSE](LICENSE).
