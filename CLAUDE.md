# CLAUDE.md — Chiliz Chain Client

## What this repo is

This repository is **Chiliz Chain v2 (CC2)** — the official chain client for [Chiliz Chain](https://www.chiliz.com/chiliz-chain/), an EVM-compatible Layer 1 focused on sports & entertainment, whose native token is **CHZ**.

It is a **fork of [bnb-chain/bsc](https://github.com/bnb-chain/bsc)** (the BNB Smart Chain client, itself a fork of go-ethereum). We periodically merge upstream BSC tags to pick up bug fixes, performance improvements, and protocol upgrades, while preserving a set of Chiliz-specific customisations described below.

- Fork versioning is independent: this client is versioned `2.x.y` (see `params/version.go`), unrelated to BSC's `1.x.y` tags.
- The upstream remote is usually configured as `bsc` (`https://github.com/bnb-chain/bsc.git`); upstream tags (e.g. `v1.7.3`) are fetched locally.
- Upstream syncs are done **tag by tag** (`git merge <tag>` for each intermediate tag), resolving conflicts at each step. See "Historical conflict patterns" at the bottom.
- To see the full current divergence from upstream: `git diff <last-merged-bsc-tag>...HEAD --stat`.

### Networks

| Network | Flag | Chain ID | Epoch | Genesis config |
|---|---|---|---|---|
| Chiliz mainnet | `--chiliz` | 88888 | 28,800 | `config/embedded/chiliz.json` |
| Spicy (testnet) | `--spicy` | 88882 | 7,200 | `config/embedded/spicy.json` |
| Scoville (legacy testnet) | `--scoville` | 88880 | 1,200 | `config/embedded/scoville.json` |

Chiliz has its own hardfork schedule on top of the Ethereum/BSC forks, with both block-based forks (`RuntimeUpgradeBlock`, `DeployOriginBlock`, `DeploymentHookFixBlock`, `DeployerFactoryBlock`) and timestamp-based forks (`Dragon8Time`, `Dragon8FixTime`, `Snake8Time`, `Pepper8Time`, `Pipe8Time`).

---

## Invariants — Chiliz-specific code that must NEVER be lost in an upstream merge

When resolving merge conflicts, the Chiliz side of these areas must be preserved (while still integrating upstream's surrounding changes). Losing any of these is a consensus break.

### 1. EVM hooks — deployment control & contract registration

Chiliz intercepts contract deployment and invocation in the EVM and delegates permission checks to the **DeployerProxy system contract** (`0x...7005`).

- `core/vm/chiliz.go` — `applyChilizInvocationEvmHook()` (calls `checkContractActive`) and `applyChilizDeploymentEvmHook()` (calls `registerDeployedContract`), plus `deployerProxyHooksActive()` — the single predicate gating both hooks: `HasRuntimeUpgrade && !DeployerProxySunset` (on after the RuntimeUpgrade fork, which also keeps hooks out of pre-fork / non-Parlia upstream test fixtures; off again after the `DeployerProxySunset` timestamp fork). **These hooks must never be removed, even after `DeployerProxySunset` is active.** mainnet and spicy are live chains: every historical block produced in the `RuntimeUpgrade → DeployerProxySunset` window executed these hooks, so re-syncing/replaying that range requires the exact same code path — deleting it would be a consensus break on historical blocks. The predicate (not deletion) is what turns the behavior off for new blocks. Don't let an upstream merge resurrect a hardcoded `HasRuntimeUpgrade`/`DeployerProxySunset` gate; the only gate is `deployerProxyHooksActive()`.
- `core/vm/evm.go` — every `evm.precompile(addr)` call upstream is replaced by `evm.precompileOrHook(addr, caller)` in `Call`, `CallCode`, `DelegateCall`, `StaticCall`; hook insertion points exist in `Call()` and `create()`, each gated by `evm.deployerProxyHooksActive()` (the two `create()` sites additionally branch on the `HasDeploymentHookFix` rule, which is a *timing* selector — pre- vs post-collision-check placement — not a gate); a `hookStateDBAdapter` wraps StateDB for hook calls.
- `core/vm/systemcontract/` — hook factory, types, errors, and the **runtime upgrade hook** (`upgrade.go`): the `RuntimeUpgradeContract` (`0x...7004`) can replace system contract bytecode in place via `upgradeTo(address,bytes)` at hook address `0x...7f01`, gated by the `HasRuntimeUpgrade` rule.
- `common/systemcontract/` — the system-contract address registry (`IsSystemContract()`), the `IEvmHooks` ABI, and constants. BAS system contracts live at `0x...7001`–`0x...7006` (staking, governance, chain config, runtime upgrade, deployer proxy, tokenomics). System contracts bypass all hooks.
- `core/vm/contracts.go` — `RunPrecompiledContract()` copies the input buffer for zero-gas contracts (EVM hooks consume 0 gas).
- Deployer attribution: `registerDeployedContract` is passed `tx.Origin` vs the direct caller depending on the `HasDeployOrigin` / `DeployerFactory` rules.

**Upstream merges touch these files constantly. Any upstream refactor of `EVM.Call`/`create`/precompile dispatch must be re-wired through `precompileOrHook` and the hook insertion points.**

### 2. Consensus (Parlia) — tokenomics, inflation, and Chiliz hardforks

`consensus/parlia/` is the highest-risk merge area. Chiliz-specific behaviour:

- **Dragon8 / Dragon8Fix — per-block CHZ inflation.** `parlia.go`: `getInflationPct()` (exponential decay `9.24·e^(−0.25·year)+1.60`, floor 1.88% after year 13), `getNewSupplyForBlock()`, `getNewSupplyForBlockDragon8Fix()` (hardcoded 14-year schedule), `getLastSupplyFromTokenomics()`, and `distributeToTokenomics()` which deposits the minted amount + gas fees into the **Tokenomics contract** (`0x...7006`) via `deposit(validator, newTotalSupply, inflationPct)`. When Dragon8 is active, BSC's system-reward distribution is skipped entirely.
- **Pepper8 / Pipe8 — one-time mints.** `pepper8Fork.go` / `pipe8Fork.go` + `common/pepper8/` / `common/pipe8/`: at each fork's activation block, a fixed amount of CHZ (148.6M for Pepper8, ~455.7M for Pipe8) is minted to the coinbase and forwarded by a zero-gas-price system transaction to the recipient address (`0xE0d17A41C1A4Fe527e375C644F9D2A02e96111ED`). Pepper8 also deploys the deterministic deployment proxy at `0x4e59b44847b379578588920cA78FbF26c0B4956C`.
- **Snake8 — validator frequency data in headers.** Changes the header `Extra` format (adds a `"VFQ"`-prefixed section with parent timestamp + RLP frequency data), rewrites `getValidatorBytesFromHeader()`, changes the `snapshot()` signature to take `(…, isSnake8Fork, extraHeader)` (25+ call sites including all of `api.go`), adds `IsSnake8Fork`/`FrequencyRLP` to the `Snapshot` struct, and forces `TurnLength = 50` when active.
- **`distributeIncoming()`** is the single most conflict-prone function — it contains the Dragon8/Pepper8/Pipe8 branching on top of BSC's reward logic.
- `isToSystemContract()` uses the shared `common/systemcontract` registry plus the Pepper8/Pipe8 recipient addresses, instead of BSC's hardcoded map.
- `systemRewardPercent = 5` (BSC uses 4); `defaultEpochLength` is a `var` overridden from the genesis Parlia config, not a const.
- The `consensus.PoSA` interface (`consensus/consensus.go`) gains Chiliz methods: `IsPepper8Deposit/Block`, `GetPepper8MintAmount`, `IsPipe8Deposit/Block`, `GetPipe8MintAmount`, `IsTokenomicsDeposit`.
- **EIP-1559 base fee floor**: `consensus/misc/eip1559/eip1559.go` clamps the base fee at `InitialBaseFee` instead of letting it decay to 0.

### 3. Gas & fee rules

- `params/protocol_params.go`: `InitialBaseFee = 2_500_000_000_000` (2,500 Gwei, vs 1 Gwei upstream), `BlobTxMinBlobGasprice = 150_000_000_000`, blob retention divisors changed (0.45 → 3, ~6 days retention).
- `core/state_transition.go`: zero-gas-price system transactions and Pepper8/Pipe8 deposits from the coinbase are exempt from the `GasFeeCap >= BaseFee` check; the effective tip is clamped at 0 to avoid negative-tip arithmetic.
- `eth/gasprice/gasprice.go`: `DefaultMaxPrice = 20000 GWei`.

### 4. Chain config & networks

- `params/config.go`: Chiliz fork fields on `ChainConfig` (block-based: `RuntimeUpgradeBlock`, `DeployOriginBlock`, `DeploymentHookFixBlock`, `DeployerFactoryBlock`; timestamp-based: `Dragon8Time`, `Dragon8FixTime`, `Snake8Time`, `Pepper8Time`, `Pipe8Time`, `DeployerProxySunsetTime`), their `Is<Fork>()` helpers, and the corresponding flags on the `Rules` struct (`Dragon8`, `Dragon8Fix`, `Snake8`, `HasRuntimeUpgrade`, `HasDeployOrigin`, `HasDeploymentHookFix`, `DeployerFactory`, `DeployerProxySunset`).
- `config/embeded.go` + `config/embedded/*.json`: compile-time-embedded genesis configs for the three networks.
- `params/bootnodes.go`: `ChilizMainnetBootnodes`, `ChilizSpicyBootnodes`, `ChilizScovilleBootnodes`.
- `params/version.go`: independent Chiliz versioning (`2.x.y`) — never take upstream's version numbers.
- `.gitmodules` / `genesis/`: submodule pointing to `chiliz-chain/v2-genesis-config` (genesis contracts/state maintained outside this repo).
- `cmd/utils/flags.go` + `cmd/geth/config.go`: `--chiliz`, `--spicy`, `--scoville`, `--genesis` flags and the network-selection / embedded-genesis loading logic.

### 5. Tracing & state access consistency

The custom mints (Tokenomics/Pepper8/Pipe8 deposits) must be mirrored anywhere state is re-executed outside consensus, or traces/state diverge:

- `eth/tracers/api.go` — after system txs, checks `IsTokenomicsDeposit()/IsPepper8Deposit()/IsPipe8Deposit()` and credits the coinbase.
- `eth/state_accessor.go` — same deposit handling.
- `eth/tracers/native/prestate.go` + `eth/tracers/js/internal/tracers/prestate_tracer.js` — prestate tracer additions.

**If upstream adds a new code path that replays transactions (new tracer API, simulation endpoint…), the deposit handling must be added there too.**

### 6. Misc behavioural tweaks

- `eth/sync.go`: `defaultMinSyncPeers = 1` (vs 5).
- `p2p/dnsdisc/sync.go`: empty-slice guard on `ct.links.missing`.
- `consensus/parlia/parlia.go`: +1 s tolerance on the future-block-time check.
- `core/state/`: exported `StateObject` alias and `GetOrNewStateObject()` (used by the runtime-upgrade hook).
- `core/types/transaction.go`: public `EffectiveGasPrice()` helper.

---

## Safe-to-override areas — upstream almost always wins

Take the upstream side by default in these areas (still build + test afterwards):

- General EVM/protocol upgrades (new EIPs, opcode changes, gas schedule updates other than the constants listed above).
- Performance work: trie/snapshot/freezer/database layers, txpool internals, p2p protocol internals.
- Go toolchain bumps and `go.mod` dependency updates.
- New upstream features that don't touch the files listed in the invariants section.
- `eth/tracers` internals *except* the deposit-handling blocks listed above.

## Ours-entirely areas — ignore upstream, keep ours

- `.github/workflows/` (Chiliz CI: `build.yml`, `deploy.yml` → AWS ECR; upstream workflows were deleted). **Only the Chiliz-owned workflows belong here** — currently `build.yml`, `build-test.yml`, `deploy.yml`, `docker-release.yml`, `evm-tests.yml`, `lint.yml`, `nancy.yml`, `pre-release.yml`, `release.yml`, `unit-test.yml`. Upstream BSC/go-ethereum workflows must be **deleted on every sync**. They re-appear two ways: a *new* upstream workflow merges in silently (no conflict, since the merge base lacks it — e.g. `validate_pr.yml` from go-ethereum PR #32480 slipped in during the v1.6.3→v1.7.3 sync), or a previously-deleted one comes back as a modify/delete conflict (keep it deleted). Known files to drop: `validate_pr.yml`, `commit-lint.yml`, `integration-test.yml`. After any BSC merge, diff `.github/workflows/` against `develop` and `git rm` anything not in the Chiliz-owned list above.
- `Dockerfile*`, `docker-compose*.yaml`, `dc.yaml`, `docker/`, `Makefile` docker targets, `prometheus.yml`, `.env.faucet`.
- `cmd/faucet/` customisations, `README.md`, `CHANGELOG.md`, `HOWTO.md`.

## Grey areas — resolve with low confidence, flag for human review

- Any upstream change to `consensus/parlia/parlia.go` that overlaps `distributeIncoming()`, `snapshot()`, header `Extra` parsing/validation, or the system-transaction detection path.
- Upstream changes to `EVM.Call`/`create`/precompile dispatch in `core/vm/evm.go`.
- Upstream changes to fee validation in `core/state_transition.go` or base-fee calculation in `consensus/misc/eip1559/`.
- New BSC hardforks: they must be added to `params/config.go` *alongside* the Chiliz fork fields, and their activation for Chiliz networks is a **business decision** — never schedule them in `config/embedded/*.json` without explicit instruction.
- Changes to `consensus/parlia/abi.go` (Chiliz uses different system-contract ABIs than BSC in places, plus the Tokenomics ABI).

---

## Verification after a merge

- `make geth` must build.
- `go test ./consensus/parlia/... ./core/vm/... ./params/...` as a minimum smoke set; full `make test` has known pre-existing failures unrelated to merges (see notes below).
- Known pre-existing failure: `TestParlia_applyTransactionTracing` fails on develop independently of upstream merges (Feynman-related, disabled in our config).

## Historical conflict patterns

Keep this log updated after every upstream sync — it is precedent for future merges.

- **v1.6.3 → v1.7.3 sync (COR-27, June 2026):** merged tag by tag (`v1.7.0-alpha` → `v1.7.1` → `v1.7.2` → `v1.7.3`). Heaviest conflicts in `core/vm/evm.go` (upstream refactors around call/create paths vs our hooks — the `hookStateDBAdapter` needed re-adaptation) and `consensus/parlia/` (reward distribution and snapshot changes). The repo history before this sync was squash-based, so a graft/replace-based approach was used to give git a usable merge base — see the team's sync runbook / COR-27 notes.
