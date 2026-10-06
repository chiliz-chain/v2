# How this chain has actually been broken

Every entry below shipped past a green build and a green test suite. They are written as **shapes**
— the pattern to match on a new diff — rather than as history. If a change in review rhymes with
one of these, say so and name the precedent.

---

## Shape 1 — every line preserved, two of them reordered

**COR-37, upstream v1.7.6 sync.** Upstream split `Prepare` into `Prepare` + `SetExtraData`. The
merge resolution faithfully kept every Chiliz line, but the Snake8 frequency computation ended up
running *after* difficulty stamping instead of before. Under Snake8 the verifier judges
`header.Difficulty` against the frequency data embedded in the same header, so producers began
emitting blocks that were invalid by construction. The chain forked. Found on a devnet, not by a
test.

**Match on:** an upstream refactor that splits or reorders a function, where a Chiliz computation
and its consumer end up on opposite sides of the new boundary. Presence of every line is not
evidence of correctness — trace the dataflow. Ask *what reads this value, and does it still run
after the value is produced?*

**Also:** unit tests and a clean build do not exercise multi-node block production. A PR touching
`Prepare`, `Seal`, `snapshot()` or worker ordering needs a multi-validator devnet run before
release, and it is fair to say so in review.

---

## Shape 2 — a node-local failure returning an error from consensus

**COR-184.** `getLastSupplyFromTokenomics()` resolved its state read by block *number* rather than
by parent hash. By-number resolution goes through the canonical number→hash index and returns
`"header not found"` whenever that index does not currently cover the parent height — a purely
node-local condition. Because an error out of `Finalize` is a bad-block verdict, a valid mainnet
block (35,890,218) was condemned on one RPC node for three weeks.

**Match on:** any state read in the consensus path resolved by number instead of by hash
(`BlockNumberOrHashWithHash(parent…)` or `LatestBlockNumber` are the safe forms); and, more
generally, any path where an RPC timeout, a gas cap, a pruned ancient or a missing historical
state can turn into a returned error inside `Finalize` or `verify*`.

The corollary is not "always fail open". The two cases differ by what the value feeds:

- **Fail open** when the data is recomputable verification data and a local read failure would
  otherwise reject the honest head — e.g. Snake8Fix's `errSnake8StakeLookup` in
  `verifySnake8FrequencyData()`.
- **Fail hard** when the value feeds the mint, because failing open would diverge consensus — e.g.
  the Dragon8 supply read.

A diff that "unifies" these two is a finding. So is one that tightens the verifier or loosens the
sealer.

---

## Shape 3 — the bare line with no surrounding structure

**COR-193.** `evm.warmDeployerProxyOnHookDispatch(addr)` is a single statement in `EVM.Call`, right
after the `precompileOrHook` dispatch, with no `if`, no block, nothing to anchor it. An upstream
call-path refactor drops it without producing a conflict. Losing it made mainnet block 31,384,697
and spicy 32,196,267 unimportable — a full resync stops dead.

**Match on:** Chiliz one-liners embedded in upstream functions. They survive merges only by luck.
`TestReplayFixtures` is the net that catches this class; a diff that changes replay behaviour and
also edits `internal/replay/testdata` has caught itself and papered over it.

---

## Shape 4 — the constant that compiles and is silently wrong

**COR-214.** Chiliz fee values live in `ForBSC`-suffixed constants
(`InitialBaseFeeForBSC` = 2,500 Gwei, `BlobTxMinBlobGaspriceForBSC` = 150 Gwei); the unsuffixed
names hold upstream's defaults (1 Gwei, 1 wei) for non-Parlia chains. Reaching for the unsuffixed
name on a Parlia path type-checks perfectly — and the magnitude differs per constant: 2500x for the
base fee, 1.5e11x for the blob gas price. A fuzz corpus was seeded that way and explored the
wrong space entirely.

**Match on:** `InitialBaseFee`, `BlobTxMinBlobGasprice` and their kin used anywhere a Parlia chain
config can reach — production code, tests, fuzz seeds, tooling. The call-site selector is
`config.Parlia != nil` or a helper like `MinBlobGasprice(config)`.

---

## Shape 5 — the clean merge that breaks startup

**PR #51, v1.7.3→v1.7.6 sync.** The one real conflict in that sync was resolved correctly. The ship
blocker came from a file that merged *cleanly*: upstream's new bid-block MEV feature (BEP-675) made
`parlia.New()` assert validator-set ABI selectors that the Chiliz BAS ABI does not have, panicking
at every node startup. Fixed by fork-gating the assertion on
`PlatoBlock != nil && FeynmanTime != nil` and skipping the four upstream tests that exercise it.

**Match on:** new upstream code that assumes BSC's system contracts, BSC's fork schedule or BSC's
ABIs, arriving with no conflict because the merge base never had it. **The bulk of a release lands
without conflict and is the easy thing to miss.** And never resolve this class by adding the
missing methods to the BAS ABI to appease the assertion — the contracts genuinely lack them.

Two related items from the same sync, both un-fork-gated upstream checks that retroactively change
replay semantics on Parlia chains: `ErrCoinbaseAsContract` in `core/vm/interpreter.go`, and the
switch of `IsSystemTransaction` to `EffectiveGasPriceForBSC()`.

---

## Shape 6 — the version that does not match its tag

**PR #65.** Releases are tagged bare `X.Y.Z`, the release workflow names the release from the git
tag, and the binary self-reports from `version/version.go` and `params/version.go`. A release-bound
PR whose compiled-in version already had a published tag shipped without a bump and needed a
last-minute manual commit.

**Match on:** a release-bound PR that touches neither version file, or touches one and not the
other. Upstream syncs get this automatically from `run-sync.sh`, which reports the outcome in the
PR body; hand-written PRs do not.

---

## Shape 7 — deleting code that looks dead

The DeployerProxy EVM hooks are inert once `DeployerProxySunset` is active. The pre-Dragon8Fix
supply read is dead on mainnet. Neither can be removed: live chains replayed those fork windows,
and a resync must execute the identical path. Fork-window code is switched off by its predicate,
never by deletion.

**Match on:** any "this is unreachable / unused, remove it" suggestion in `core/vm/` or
`consensus/parlia/`. Check whether a *historical* block could have reached it.
