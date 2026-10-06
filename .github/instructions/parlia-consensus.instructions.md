---
applyTo: "consensus/parlia/**,consensus/misc/**,consensus/consensus.go,miner/**"
---

# Reviewing Parlia consensus

This is the highest-risk area in the repository. An error out of `Finalize` is a **bad-block
verdict**, so a change that makes a node-local failure return an error can condemn a perfectly
valid block that the rest of the fleet accepted.

## Dragon8 — per-block CHZ inflation

- `distributeIncoming()` carries the Dragon8 / Pepper8 / Pipe8 branching on top of BSC's reward
  logic. It is the single most conflict-prone function here. Any upstream restructuring of it
  needs the Chiliz branches checked one by one, and checked for **order**, not just presence.
- `getLastSupplyFromTokenomics()` must resolve its `eth_call` with
  `rpc.BlockNumberOrHashWithHash(header.ParentHash, false)` — **never by block number**. Resolving
  by number goes through the canonical number→hash index and returns `"header not found"` whenever
  that index does not currently cover the parent height, a purely node-local condition. Flag any
  by-number state read anywhere in the consensus path; this was the only one that ever existed
  (COR-184) and it condemned a valid mainnet block on one RPC node for three weeks.
- That call must stay inside the `else if IsDragon8(...)` branch. Do not let it be hoisted above
  the branch: `getNewSupplyForBlockDragon8Fix()` takes only `(forkTime, currentTime)` and never
  consumes `lastSupply`, so on mainnet (no `dragon8Time`) a hoisted call would run every block and
  condemn blocks over a value that is then discarded.
- It must stay **hard-fail** in that branch — the value feeds the mint, so failing open would
  diverge consensus. This is the opposite of the Snake8Fix rule below; do not "unify" them.

## Snake8 — validator frequency data in the header

- In `prepare()`, `refreshFrequencyRLP()` must run **before** `header.Difficulty = calcDifficulty(...)`
  and before the Ramanujan backoff, and `SetExtraData` must embed the same deterministic bytes.
  Verifiers judge `header.Difficulty` against the frequency data embedded in that same header, so
  stamping difficulty first makes producers emit blocks that are invalid by construction and forks
  the chain. Flag any reordering (COR-37 arrived exactly this way, through an upstream
  `Prepare`/`SetExtraData` split, with every Chiliz line intact).
- Difficulty must be computed for `header.Coinbase`, not `p.val`, so `PrepareForBidBlock` stays
  coherent.
- `TestPrepareDifficultyMatchesEmbeddedFrequencyData` pins this. If a diff changes that test,
  that is the finding.
- **This extends into `miner/`.** Block production ordering lives partly in `miner/worker.go` and
  the bid-block files (`miner/bid_block.go`, `miner/bid_block_permission.go`), so a diff that only
  reorders the worker is the same COR-37 shape with none of the consensus files touched. Any change
  to `Prepare` / `Seal` / `snapshot()` / worker ordering also means the release needs a
  multi-validator devnet run — say so in the review.

## Snake8Fix — verified frequency data (COR-173)

- Verification lives in `Finalize` → `verifySnake8FrequencyData()`, not in `verifyCascadingFields`.
  Flag a refactor of `Finalize` that drops the call.
- Verification **fails open** on `errSnake8StakeLookup` (RPC timeout, gas cap, missing historical
  state during tracing replay) and only a byte mismatch
  (`errMismatchedSnake8FrequencyData`) condemns a block. The **sealer** side stays hard-fail, so
  honest producers never emit unverifiable blocks. Flag any change that tightens the verifier or
  loosens the sealer.
- Post-fork bytes are a pure function of the parent hash and are memoized in `frequencyCache`. The
  pre-fork latest-state path must **never** be cached, and the pre-fork acceptance path must never
  be tightened retroactively — historical bytes were produced from latest state, and "fixing" that
  rewrites history.

## Other pins

- The bid-block (BEP-675) `signableSystemTxSelectors` assertion loop in `New()` must stay wrapped
  in `if chainConfig.PlatoBlock != nil && chainConfig.FeynmanTime != nil`. Ungated, it panics at
  every node startup because the Chiliz BAS validator-set ABI lacks `distributeFinalityReward` and
  `updateValidatorSetV2`. Flag any resurrection of the unconditional loop — and never resolve it
  by adding those methods to the ABI.
- `systemRewardPercent = 5` (BSC uses 4). `defaultEpochLength` is a `var` overridden from the
  genesis Parlia config, not a const.
- `isToSystemContract()` uses the shared `common/systemcontract` registry plus the Pepper8/Pipe8
  recipient addresses, not BSC's hardcoded map.
- **System-transaction detection is replay-critical.** `IsSystemTransaction` lives in this file, so
  a change to the predicate — for example switching it to an `EffectiveGasPriceForBSC()`-based
  check — never loads the replay instructions, yet it retroactively changes which historical
  transactions counted as system transactions. Un-fork-gated, that is a consensus break; see
  `.github/instructions/replay-consistency.instructions.md`.
- `consensus/misc/eip1559`: the base fee is clamped at `InitialBaseFeeForBSC` instead of decaying
  to zero.
- `+1s` tolerance on the future-block-time check is deliberate.
- `Parlia.snapshot()` mutates shared LRU entries in place with caller-dependent `extraHeader` data
  (COR-174 addressed the copy semantics). Any change to `snapshot()` or its caching interacts with
  this — flag it for human review rather than proposing a fix.
