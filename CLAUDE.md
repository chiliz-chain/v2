# CLAUDE.md — Chiliz Chain Client

> **Reviewing a PR rather than merging one?** The same material, rewritten as review checks, lives
> in `.github/skills/code-review/` (invariants, past regression shapes, how to review an upstream
> sync) with per-path rules in `.github/instructions/`. Those files are also what GitHub Copilot
> code review reads. Keep them in sync with this file — see `docs/copilot-code-review.md`.

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

Chiliz has its own hardfork schedule on top of the Ethereum/BSC forks, with both block-based forks (`RuntimeUpgradeBlock`, `DeployOriginBlock`, `DeploymentHookFixBlock`, `DeployerFactoryBlock`) and timestamp-based forks (`Dragon8Time`, `Dragon8FixTime`, `Snake8Time`, `Snake8FixTime`, `Pepper8Time`, `Pipe8Time`, `DeployerProxySunsetTime`).

---

## Invariants — Chiliz-specific code that must NEVER be lost in an upstream merge

When resolving merge conflicts, the Chiliz side of these areas must be preserved (while still integrating upstream's surrounding changes). Losing any of these is a consensus break.

### 1. EVM hooks — deployment control & contract registration

Chiliz intercepts contract deployment and invocation in the EVM and delegates permission checks to the **DeployerProxy system contract** (`0x...7005`).

- `core/vm/chiliz.go` — `applyChilizInvocationEvmHook()` (calls `checkContractActive`) and `applyChilizDeploymentEvmHook()` (calls `registerDeployedContract`), plus `deployerProxyHooksActive()` — the single predicate gating both hooks: `HasRuntimeUpgrade && !DeployerProxySunset` (on after the RuntimeUpgrade fork, which also keeps hooks out of pre-fork / non-Parlia upstream test fixtures; off again after the `DeployerProxySunset` timestamp fork). **These hooks must never be removed, even after `DeployerProxySunset` is active.** mainnet and spicy are live chains: every historical block produced in the `RuntimeUpgrade → DeployerProxySunset` window executed these hooks, so re-syncing/replaying that range requires the exact same code path — deleting it would be a consensus break on historical blocks. The predicate (not deletion) is what turns the behavior off for new blocks. Don't let an upstream merge resurrect a hardcoded `HasRuntimeUpgrade`/`DeployerProxySunset` gate; the only gate is `deployerProxyHooksActive()`.
- `core/vm/evm.go` — every `evm.precompile(addr)` call upstream is replaced by `evm.precompileOrHook(addr, caller)` in `Call`, `CallCode`, `DelegateCall`, `StaticCall`; hook insertion points exist in `Call()` and `create()`, each gated by `evm.deployerProxyHooksActive()` (the two `create()` sites additionally branch on the `HasDeploymentHookFix` rule, which is a *timing* selector — pre- vs post-collision-check placement — not a gate); a `hookStateDBAdapter` wraps StateDB for hook calls. **`Call` also carries a bare `evm.warmDeployerProxyOnHookDispatch(addr)` line right after the `precompileOrHook` dispatch (COR-193)** — one line with no surrounding structure, exactly the shape an upstream call-path refactor drops silently; losing it makes mainnet 31,384,697 and spicy 32,196,267 unimportable again. `TestReplayFixtures` catches it.
- **The asymmetry between the four call sites is deliberate and load-bearing**: all four dispatch through `precompileOrHook`, but only `Call` carries the *invocation* hook and only `Call` carries the COR-193 warm-up. `CallCode`, `DelegateCall` and `StaticCall` never consult the DeployerProxy about their callee. An upstream refactor that "completes the set" by adding the hook to the other three does **not** show up as a simple gas delta — `applyChilizInvocationEvmHook` returns `gas` unchanged on purpose (`// don't charge gas for this interceptor to let simple send be 21000 gas`). What it does instead: the hook's inner call warms `0x…7005` in the EIP-2929 access list, so a *later* access in the same transaction is charged warm instead of cold (unless that frame reverts — the warmth is journaled with it) — the COR-193 mechanism exactly — and it lets the DeployerProxy answer `ErrNotAllowed`, reverting calls the chain historically executed. Sampling blocks for gas differences will therefore give a false all-clear; replay is what catches it. `FuzzHookDispatchVariants` (`core/vm/runtime/chiliz_dispatch_fuzz_test.go`, COR-233) pins both halves. Note the dispatch itself is *not* uniformly safe: `StaticCall` still routes to the state-writing runtime-upgrade hook at `0x…7f01`, so a static frame can mutate state (COR-244, filed — observed behaviour, not intent; any fix is a consensus change and needs a fork gate). **Reachable only when the immediate caller is `0x…7004`**: `upgrade.go` returns `errInvalidCaller` for everyone else, so the one live shape is the RuntimeUpgrade contract STATICCALLing its own hook — not any contract writing state through a static frame. Size it accordingly.
- `core/vm/systemcontract/` — hook factory, types, errors, and the **runtime upgrade hook** (`upgrade.go`): the `RuntimeUpgradeContract` (`0x...7004`) can replace system contract bytecode in place via `upgradeTo(address,bytes)` at hook address `0x...7f01`, gated by the `HasRuntimeUpgrade` rule.
- `common/systemcontract/` — the system-contract address registry (`IsSystemContract()`), the `IEvmHooks` ABI, and constants. BAS system contracts live at `0x...7001`–`0x...7006` (staking, governance, chain config, runtime upgrade, deployer proxy, tokenomics). System contracts bypass all hooks.
- `core/vm/contracts.go` — `RunPrecompiledContract()` copies the input buffer for zero-gas contracts (EVM hooks consume 0 gas).
- Deployer attribution: `registerDeployedContract` is passed `tx.Origin` vs the direct caller depending on the `HasDeployOrigin` / `DeployerFactory` rules.

**Upstream merges touch these files constantly. Any upstream refactor of `EVM.Call`/`create`/precompile dispatch must be re-wired through `precompileOrHook` and the hook insertion points.**

### 2. Consensus (Parlia) — tokenomics, inflation, and Chiliz hardforks

`consensus/parlia/` is the highest-risk merge area. Chiliz-specific behaviour:

- **Dragon8 / Dragon8Fix — per-block CHZ inflation.** `parlia.go`: `getInflationPct()` (exponential decay `9.24·e^(−0.25·year)+1.60`, floor 1.88% after year 13), `getNewSupplyForBlock()`, `getNewSupplyForBlockDragon8Fix()` (hardcoded 14-year schedule), `getLastSupplyFromTokenomics()`, and `distributeToTokenomics()` which deposits the minted amount + gas fees into the **Tokenomics contract** (`0x...7006`) via `deposit(validator, newTotalSupply, inflationPct)`. When Dragon8 is active, BSC's system-reward distribution is skipped entirely.
- **Dragon8 supply read — parent-hash pinned and branch-confined (COR-184).** `getLastSupplyFromTokenomics()` resolves its `eth_call` with `BlockNumberOrHashWithHash(header.ParentHash, false)`, **never** by block number. Resolving by number goes through the canonical number->hash index and returns `"header not found"` (`StateAndHeaderByNumber`) whenever that index does not currently cover the parent height — a purely node-local condition that, because an error out of `Finalize` is a bad-block verdict, condemned a valid mainnet block (35,890,218) on one RPC node for three weeks. It is also the **only** by-number state read that ever existed in the consensus path; every other read is hash-pinned or `LatestBlockNumber`, so `"header not found"` in a BAD BLOCK dump is a unique fingerprint for this call. The call is additionally confined to the `else if IsDragon8(...)` branch of `distributeIncoming()`, because only the pre-Dragon8Fix schedule consumes `lastSupply`; `getNewSupplyForBlockDragon8Fix()` takes only `(forkTime, currentTime)`. Do not hoist it back above the branch (mainnet has `dragon8Time` absent, so it would run every block and its failure would condemn blocks over a value that is discarded) and do not delete it (spicy still replays its historical `dragon8Time`→`dragon8FixTime` window, where the value is consensus-critical). It must stay **hard-fail** there — unlike COR-173's recomputable verification data, this value feeds the mint, so failing open would diverge consensus.
- **Pepper8 / Pipe8 — one-time mints.** `pepper8Fork.go` / `pipe8Fork.go` + `common/pepper8/` / `common/pipe8/`: at each fork's activation block, a fixed amount of CHZ (148.6M for Pepper8, ~455.7M for Pipe8) is minted to the coinbase and forwarded by a zero-gas-price system transaction to the recipient address (`0xE0d17A41C1A4Fe527e375C644F9D2A02e96111ED`). Pepper8 also deploys the deterministic deployment proxy at `0x4e59b44847b379578588920cA78FbF26c0B4956C`.
- **Snake8 — validator frequency data in headers.** Changes the header `Extra` format (adds a `"VFQ"`-prefixed section with parent timestamp + RLP frequency data), rewrites `getValidatorBytesFromHeader()`, changes the `snapshot()` signature to take `(…, isSnake8Fork, extraHeader)` (25+ call sites including all of `api.go`), adds `IsSnake8Fork`/`FrequencyRLP` to the `Snapshot` struct, and forces `TurnLength = 50` when active.
- **Snake8 producer coherence — frequency data BEFORE difficulty.** Under Snake8, in-turn selection depends on the snapshot's `FrequencyRLP`, and verifiers judge `header.Difficulty` against the frequency data *embedded in that same header*. So in `prepare()`, `refreshFrequencyRLP()` must run **before** `header.Difficulty = calcDifficulty(...)` and before the Ramanujan backoff, and `SetExtraData` must embed the same (deterministic) data. An upstream refactor that moves difficulty stamping ahead of the frequency refresh makes producers emit blocks that are invalid by construction (diff=1 while the embedded data says in-turn) and **forks the chain** — this exact regression came in with upstream v1.7.6's `Prepare`/`SetExtraData` split (COR-37) and was caught on a devnet, not by unit tests. Difficulty must be computed for `header.Coinbase` (not `p.val`) so `PrepareForBidBlock` stays coherent too. `TestPrepareDifficultyMatchesEmbeddedFrequencyData` pins this ordering — a merge that breaks it must fix the ordering, not the test.
- **Snake8Fix (COR-173) — embedded frequency data is consensus-verified.** Post-`Snake8FixTime` (gated on **parent**.Time, like Snake8 itself): producers pin stake reads to the parent block's state (`refreshFrequencyRLP` passes a `BlockNumberOrHashWithHash(parent.Hash(), false)` to `getValidatorTotalDelegated` — the `RequireCanonical=false` half matters as much as the hash: `eth/api_backend.go` answers a `RequireCanonical` read with `"hash is not currently canonical"` whenever the parent is not on the node's current canonical chain, which is exactly the case pinning exists to serve; pre-fork it stays `nil` = latest — do not "fix" the pre-fork path, historical bytes were produced from latest state), a sealer that cannot read stakes **refuses to seal** (hard error from `prepare`/`SetExtraData`/`PrepareForBidBlock`), and verifiers recompute the bytes from parent state in `Finalize` → `verifySnake8FrequencyData()` and reject mismatches (`errMismatchedSnake8FrequencyData`). Verification **fails open** on node-local read failures (`errSnake8StakeLookup`: RPC timeout, gas cap, missing historical state during tracing replay) — only a byte mismatch condemns a block, because an error out of `Finalize` is a bad-block verdict and a local condition must not reject the honest head; the sealer side stays hard-fail, so honest producers never emit unverifiable blocks. Post-fork bytes are a pure function of the parent hash and are memoized in `frequencyCache` — the pre-fork latest-state path must never be cached. The check lives in `Finalize` (state-dependent, like `verifyValidators`), NOT in `verifyCascadingFields` — an upstream refactor of `Finalize` must keep this call, and the pre-fork acceptance path must never be tightened retroactively. The `snake8fix_test.go` suite pins producer pinning, rejection, pre-fork skip, sealer refusal, and the deterministic zero-stake degradation.
- Known tracked fragility in this area (pre-existing, do not "fix" opportunistically during a merge): `Parlia.snapshot()` mutates shared LRU entries in place with caller-dependent `extraHeader` data (COR-174 addressed the copy semantics) — an upstream refactor of `snapshot()`/caching interacts with this.
- **`distributeIncoming()`** is the single most conflict-prone function — it contains the Dragon8/Pepper8/Pipe8 branching on top of BSC's reward logic.
- `isToSystemContract()` uses the shared `common/systemcontract` registry plus the Pepper8/Pipe8 recipient addresses, instead of BSC's hardcoded map.
- `systemRewardPercent = 5` (BSC uses 4); `defaultEpochLength` is a `var` overridden from the genesis Parlia config, not a const.
- The `consensus.PoSA` interface (`consensus/consensus.go`) gains Chiliz methods: `IsPepper8Deposit/Block`, `GetPepper8MintAmount`, `IsPipe8Deposit/Block`, `GetPipe8MintAmount`, `IsTokenomicsDeposit`.
- **Bid-block (BEP-675) ABI selector assertion is fork-gated.** In `parlia.go` `New()`, the loop asserting `signableSystemTxSelectors` (`deposit`/`distributeFinalityReward`/`updateValidatorSetV2`, from `bid_block.go`) against the validator-set ABI is wrapped in `if chainConfig.PlatoBlock != nil && chainConfig.FeynmanTime != nil`. Upstream runs it unconditionally; Chiliz's BAS validator-set ABI lacks the latter two methods, so the ungated loop panics at every node startup. Those selectors are only packed under the Plato/Feynman forks, which no Chiliz network schedules. **An upstream merge must never resurrect the ungated loop, and the missing methods must never be added to the BAS ABI to appease it.** The runtime bid path reads the hardcoded selector map (not the ABI) and rejects non-BSC system-tx shapes, so bid-block stays fail-safe on Chiliz.
- **EIP-1559 base fee floor**: `consensus/misc/eip1559/eip1559.go` clamps the base fee at `InitialBaseFeeForBSC` instead of letting it decay to 0.

### 3. Gas & fee rules

- `params/protocol_params.go`: the Chiliz values live in **`ForBSC`-suffixed** constants, and the unsuffixed ones keep upstream's defaults for non-Parlia chains — `InitialBaseFeeForBSC = 2_500_000_000_000` (2,500 Gwei, vs `InitialBaseFee` = 1 Gwei upstream) and `BlobTxMinBlobGaspriceForBSC = 150_000_000_000` (vs `BlobTxMinBlobGasprice` = 1 wei upstream), selected at the call site by `config.Parlia != nil` / `MinBlobGasprice(config)`; blob retention divisors changed (0.45 → 3, ~6 days retention). **Reaching for the unsuffixed name when you mean the Chiliz value is a live trap** — it compiles, and on a Parlia chain it is wrong by 2500x (COR-214 seeded a fuzz corpus that way).
- `core/state_transition.go`: zero-gas-price system transactions and Pepper8/Pipe8 deposits from the coinbase are exempt from the `GasFeeCap >= BaseFee` check; the effective tip is clamped at 0 to avoid negative-tip arithmetic.
- `eth/gasprice/gasprice.go`: `DefaultMaxPrice = 20000 GWei`.

### 4. Chain config & networks

- `params/config.go`: Chiliz fork fields on `ChainConfig` (block-based: `RuntimeUpgradeBlock`, `DeployOriginBlock`, `DeploymentHookFixBlock`, `DeployerFactoryBlock`; timestamp-based: `Dragon8Time`, `Dragon8FixTime`, `Snake8Time`, `Snake8FixTime`, `Pepper8Time`, `Pipe8Time`, `DeployerProxySunsetTime`), their `Is<Fork>()` helpers, and the corresponding flags on the `Rules` struct (`Dragon8`, `Dragon8Fix`, `Snake8`, `Snake8Fix`, `HasRuntimeUpgrade`, `HasDeployOrigin`, `HasDeploymentHookFix`, `DeployerFactory`, `DeployerProxySunset`).
- `config/embeded.go` + `config/embedded/*.json`: compile-time-embedded genesis configs for the three networks.
- `params/bootnodes.go`: `ChilizMainnetBootnodes`, `ChilizSpicyBootnodes`, `ChilizScovilleBootnodes`.
- `version/version.go` **and** `params/version.go`: independent Chiliz versioning (`2.x.y`) — never take upstream's version numbers. Upstream moved the canonical constants to `version/version.go` in v1.7.x (every tag of the v1.7.4→v1.7.6 sync conflicted there); `params/version.go` still carries mirrored `VersionMajor/Minor/Patch` constants — keep **both** files on the Chiliz numbers and in sync with each other. **Bumping (COR-195):** releases are tagged bare `X.Y.Z` and the release workflow names the release from the git tag, while the binary self-reports from these files — so any release-bound merge (upstream sync or fix) whose current compiled-in version already has a published release tag must also bump the patch component in **both** files; the release tag must always equal the compiled-in version. `run-sync.sh` does this automatically for upstream syncs and reports the outcome in the PR body; manually-authored release-bound PRs need the same bump by hand (PR #65 shipped without it and needed a last-minute manual commit).
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
- `p2p/dnsdisc/sync.go`: a Chiliz `len(ct.links.missing) == 0` check in `syncNextLink` — **not an
  invariant.** It sits after the `ct.links.missing[0]` it would protect, and the only caller reaches
  it only when `!ct.links.done()` holds (`done()` is `len(missing) == 0`), so it is unreachable and
  guards nothing. Do not restore it during a merge; it can be dropped freely. Making it a real
  guard means moving it above the index, which is a code change, not a merge decision.
- `consensus/parlia/parlia.go`: +1 s tolerance on the future-block-time check — written as `time.Second.Milliseconds()/1000`, which does not *look* like a deliberate tolerance and is exactly the shape a merge drops. `FuzzHeaderTiming` (COR-236) pins it **by value**, measuring the largest accepted offset rather than reading the expression, so both dropping and widening it fail.
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

- `.github/workflows/` (Chiliz CI: `build.yml`, `deploy.yml` → AWS ECR; upstream workflows were deleted). **Only the Chiliz-owned workflows belong here** — currently `bsc-upstream-sync.yml`, `build.yml`, `build-test.yml`, `deploy.yml`, `docker-release.yml`, `evm-tests.yml`, `fuzz.yml`, `lint.yml`, `nancy.yml`, `pre-release.yml`, `release.yml`, `unit-test.yml`. Upstream BSC/go-ethereum workflows must be **deleted on every sync**. They re-appear two ways: a *new* upstream workflow merges in silently (no conflict, since the merge base lacks it — e.g. `validate_pr.yml` from go-ethereum PR #32480 slipped in during the v1.6.3→v1.7.3 sync), or a previously-deleted one comes back as a modify/delete conflict (keep it deleted). Known files to drop: `validate_pr.yml`, `commit-lint.yml`, `integration-test.yml`. After any BSC merge, diff `.github/workflows/` against `develop` and `git rm` anything not in the Chiliz-owned list above.
- The fuzz campaign: `fuzz.yml` and `.github/scripts/fuzz/run-fuzz.sh` (COR-207), which discover and run every `Fuzz*` target in the Chiliz packages weekly; `tests/fuzzers/README.md` documents how to run one locally.
- The upstream-sync tooling itself: `.github/scripts/bsc-sync/` (run-sync.sh, detect-new-tags.sh, agent prompts), `.github/bsc-sync/last-synced-tag` (sync state — advanced by the workflow, never by upstream merges), and `docs/bsc-upstream-sync.md`.
- `Dockerfile*`, `docker-compose*.yaml`, `dc.yaml`, `docker/`, `Makefile` docker targets, `prometheus.yml`, `.env.faucet`.
- `cmd/faucet/` customisations, `README.md`, `HOWTO.md`.
- `CHANGELOG.md` — ours, with one nuance: by established practice each sync **prepends the upstream BSC release notes** for the merged tags above the existing content (v1.7.3 and v1.7.4–v1.7.6 both did). Don't delete upstream notes already in the file, and don't let upstream's version of the file replace ours wholesale.

## Grey areas — resolve with low confidence, flag for human review

- Any upstream change to `consensus/parlia/parlia.go` that overlaps `distributeIncoming()`, `snapshot()`, header `Extra` parsing/validation, or the system-transaction detection path.
- Upstream changes to `EVM.Call`/`create`/precompile dispatch in `core/vm/evm.go`.
- Upstream changes to fee validation in `core/state_transition.go` or base-fee calculation in `consensus/misc/eip1559/`.
- New BSC hardforks: they must be added to `params/config.go` *alongside* the Chiliz fork fields, and their activation for Chiliz networks is a **business decision** — never schedule them in `config/embedded/*.json` without explicit instruction.
- Changes to `consensus/parlia/abi.go` (Chiliz uses different system-contract ABIs than BSC in places, plus the Tokenomics ABI).

---

## Verification after a merge

- `make geth` must build.
- `version/version.go` and `params/version.go` must agree, and if the merge will ship, must be bumped past the last released tag (`git tag -l '2.*' | sort -V | tail -1`). The sync script bumps this automatically when needed — check the **Version** note in the PR body (bumped / left alone / ⚠️ by hand) rather than re-bumping.
- `go test ./consensus/parlia/... ./core/vm/... ./params/... ./config/... ./internal/replay/...` as a minimum smoke set (`./config` holds the embedded genesis configs and its fuzz target, and was missing from this list); full `make test` has known pre-existing failures unrelated to merges (see notes below). `TestPrepareDifficultyMatchesEmbeddedFrequencyData` (consensus/parlia) is a deliberate tripwire for the Snake8 producer-coherence invariant (§2) — if it fails after a merge, the `prepare()`/`SetExtraData` ordering was disturbed; fix the ordering, never the test.
- `TestReplayFixtures` (`internal/replay`) re-executes real historical mainnet/spicy transactions offline against their captured pre-state and pins the gas the chain charged. It is the tripwire for **any un-fork-gated change that alters gas retroactively** — the COR-193 class of bug, where a resync stops dead at a block the fleet already produced. If it fails after a merge, the merge changed replay semantics; find the change, don't retune the fixture. To widen the net after a merge, point `cmd/replaycheck` (usage: `docs/replaycheck.md`) at a live endpoint (`--upgrades`, `--governance`, or a block range): it replays those blocks with the merged build and fails on any delta. New blocks worth pinning permanently can be captured with `--save-fixture internal/replay/testdata`.
- **The fuzz targets are tripwires too, and they run as ordinary unit tests.** `go test` executes every `Fuzz*` target against its committed seed corpus without `-fuzz`, so the smoke set above runs them; the weekly campaign only explores further. **A `Fuzz*` failure means an invariant moved, not that the test is flaky** — that applies to every target, not only the ones tabulated here. The COR-229 targets map to invariants as follows; the wave-one targets (COR-207 through COR-217) cover the Snake8 `Extra` parsers, frequency math, inflation, system-transaction classification, snapshot `apply` and the hooks themselves. `go test ./... -list 'Fuzz.*'` is the authoritative list:

  | Target | Package | Guards |
  | --- | --- | --- |
  | `FuzzHookDispatchVariants` | `core/vm/runtime` | §1 — the four call sites, and that only `Call` carries the invocation hook and the COR-193 warm-up |
  | `FuzzVmRuntimeWithHooks` | `core/vm/runtime` | §1 — the hooks themselves, attribution, depth pairing |
  | `FuzzInturnSelection`, `FuzzBackOffTime` | `consensus/parlia` | §2 — the COR-37 scheduling class; **pre-Bohr** the backoff shuffle seed is pinned to `snap.Number` by value (under Bohr it is `header.Number/TurnLength`, which no Chiliz network schedules today) |
  | `FuzzSnake8ProducerVerifierRoundTrip` | `consensus/parlia` | §2 — the COR-173 round trip, parent pinning, and the fail-open/hard-fail asymmetry |
  | `FuzzVerifySnake8ExtraPair` | `consensus/parlia` | §2 — Snake8 activation: `isSnake8Enabled` must decide from the trusted parent, and `verifySnake8Extra` must reject a forged embedded parent timestamp (COR-197). The embedded value is producer-controlled and is only a provisional fallback during batch sync |
  | `FuzzDistributeIncoming` | `consensus/parlia` | §2 — mint accounting, COR-184 branch confinement, and `systemRewardPercent = 5` |
  | `FuzzHeaderTiming` | `consensus/parlia` | §6 — the +1 s tolerance, by value |
  | `FuzzSnapshotRoundTrip` | `consensus/parlia` | §2 — that `store()` **strips** `FrequencyRLP` (the COR-174 leak guard: it must not reach disk), that the loaded `IsSnake8Fork` follows the caller's argument rather than the stored blob, and the Snake8 turn-length override |
  | `FuzzCheckCompatible` | `params` | §4 — fork-schedule compatibility; carries the COR-218 expected-to-fail list |
  | `FuzzGenesisJSON` | `config` | §4 — the embedded and operator-supplied genesis parse path |

  Adding a target to a package **already listed** means bumping its line in
  `.github/scripts/fuzz/expected-targets.txt` (`run-fuzz.sh --write-baseline ./pkg`),
  or the ratchet notices. Adding one to a package that is **not** listed needs
  both: the package in `DEFAULT_PACKAGES` in `fuzz.yml` *and* a line here. Since
  COR-252 the `race` job asserts those two lists name exactly the same packages,
  so editing one alone is a hard CI failure rather than the silent miss it used
  to be — `--require-complete` only ever asserted baseline ⊆ campaign, which left
  a target in a campaign-only package (`core/state`, `eth/tracers`, …) unfuzzed
  by the ratchet and unraced, with nothing saying so.

- **Writing or repairing a fuzz target: the oracle must not read the value it pins.** A Copilot review of the COR-229 targets found four places where the expected value was computed from the same production constant the code under test uses, so changing the constant moved both and the target passed — including one that would have let an upstream merge restore BSC's `systemRewardPercent = 4` unnoticed. Expected values for consensus constants belong in the test as **golden literals**, ideally anchored to something outside the repo (a published codehash, a schedule table). The same rule applies to seeds: a property is only pinned if some seed distinguishes it from its neighbours, which is why a mutation must be shown to fail under plain `go test`, not merely under `-fuzz`.
- **`go test -race` runs in CI over the Chiliz delta packages** (the `race` job in `unit-test.yml`, COR-252). Its package list is derived from `expected-targets.txt` and cross-checked against `DEFAULT_PACKAGES` in `fuzz.yml`: the two must name exactly the same packages or the job fails with `disagree`, so **a new delta package needs a line in both files**. Adding it to one alone is a hard error rather than silently missing coverage — `--require-complete` only ever asserted baseline ⊆ campaign, so the reverse gap was invisible. **A failure there is a real finding, not flake** — the detector reports only races it actually observed, it does not guess. It is scoped to the delta rather than added to `make test` because that already takes ~32 minutes over the whole tree and race instrumentation costs 2-10x on top, and because it would surface races in upstream code with no clear owner. `TestValidatorSelectionAlgorithm` is skipped in that job alone: it simulates 10.5M blocks in a single goroutine, has no concurrency for the detector to observe, and under `-race` blows the per-package timeout on its own. It still runs unskipped in the ordinary unit-test job. This gate exists because COR-250 — a real race reachable through ordinary `Call`/`Create` under `--vm.opcode.optimize` — sat undetected until a reviewer ran `-race` by hand.
- **If the merge touched `Prepare`/`Seal`/`snapshot()`/worker ordering in any way, a multi-validator devnet run is mandatory before release** (`dc.yaml`, 4 nodes; let it run a few hundred blocks and check all heads stay in lockstep). Unit tests and a clean build do not exercise multi-node block production — the COR-37 chain split shipped through a green test suite.
- Intentionally skipped tests (COR-39 pattern): upstream tests that pack Feynman-only validator-set methods (`distributeFinalityReward`/`updateValidatorSetV2`) or construct engines from the fully-forked `ParliaTestChainConfig` for bid-block cannot pass against the Chiliz BAS ABI. They carry a `t.Skip` with a COR-39 comment (`TestParlia_applyTransactionTracing`, `TestParlia_applyTransactionModes`, `TestParliaFinalizeAndAssembleBidBlock*`, `TestParliaPrepareForBidBlock`). When an upstream merge adds a new test of this shape, skip it the same way rather than touching the ABI.

## Historical conflict patterns

Keep this log updated after every upstream sync — it is precedent for future merges.

- **Divergences we created deliberately** (expect a conflict on the next sync, keep our side):
  - `core/opcodeCompiler/compiler/opCodeProcessor.go` — `enabled` is an `atomic.Bool`, not a plain `bool` (COR-250, Sept 2026). It is written by `EVM.initNewContract` around every init-code run and read by the `taskProcessor` goroutines, with no happens-before edge to tasks already in flight; as a plain bool that is a data race `go test -race ./core/vm/...` reports against `initNewContract`. The file was otherwise untouched by Chiliz since the v1.6.3 import, so upstream will present its own version cleanly — take ours. Latent in production (`--vm.opcode.optimize` is opt-in and off in every fleet config), so the symptom on losing it is a `-race` failure, not a chain bug.

- **v1.6.3 → v1.7.3 sync (COR-27, June 2026):** merged tag by tag (`v1.7.0-alpha` → `v1.7.1` → `v1.7.2` → `v1.7.3`). Heaviest conflicts in `core/vm/evm.go` (upstream refactors around call/create paths vs our hooks — the `hookStateDBAdapter` needed re-adaptation) and `consensus/parlia/` (reward distribution and snapshot changes). The repo history before this sync was squash-based, so a graft/replace-based approach was used to give git a usable merge base — see the team's sync runbook / COR-27 notes.
- **v1.7.3 → v1.7.6 sync (COR-37, July 2026, PR #51):** first fully agent-automated sync. The one real conflict was `consensus/parlia/parlia.go` (upstream's `verifySeal`/`verifyCascadingFields` check relocation, `mining bool → systemTxMode` enum, `Prepare`/`SetExtraData` split — all merged with invariants intact). The ship blocker came from a **clean merge**, not a conflict: upstream's new bid-block MEV feature (BEP-675) made `parlia.New()` assert validator-set ABI selectors Chiliz's BAS ABI lacks, panicking every startup. Resolved by fork-gating the assertion on `PlatoBlock != nil && FeynmanTime != nil` (see invariant in §2) and COR-39-skipping the four upstream tests that exercise it. Lesson: **clean-merging upstream code can still break Chiliz at startup/consensus — always build and run the smoke set even when a tag merges without conflicts.** Also flagged for history-scan before release: upstream's un-fork-gated `ErrCoinbaseAsContract` check (`core/vm/interpreter.go`) and the `IsSystemTransaction` switch to `EffectiveGasPriceForBSC()` — both retroactively change replay semantics on Parlia chains. Second lesson, found only by running a live devnet: the agent's parlia.go resolution faithfully preserved every *piece* of Chiliz code but reordered two pieces relative to each other — upstream's `Prepare`/`SetExtraData` split moved the Snake8 frequency computation *after* difficulty stamping, making producers emit invalid-by-construction blocks and splitting the chain (see the "Snake8 producer coherence" invariant in §2). **Unit tests and a clean build do not exercise multi-node block production — an upstream sync that touches `Prepare`/`Seal`/worker ordering needs a multi-validator devnet run before release.**
