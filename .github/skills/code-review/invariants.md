# Chiliz invariants, as review questions

Each item is behaviour that exists only in this fork. Losing it — or changing when it applies — is
a consensus break. Ask these of any diff that reaches the listed files.

---

## 1. EVM hooks: deployment control and contract registration

**Files:** `core/vm/evm.go`, `core/vm/chiliz.go`, `core/vm/contracts.go`,
`core/vm/systemcontract/`, `common/systemcontract/`

Chiliz intercepts contract deployment and invocation and delegates permission checks to the
**DeployerProxy** system contract (`0x…7005`).

- Does every precompile dispatch still go through `evm.precompileOrHook(addr, caller)` — in `Call`,
  `CallCode`, `DelegateCall` **and** `StaticCall`?
- Is the bare `evm.warmDeployerProxyOnHookDispatch(addr)` line still present in `Call`, right after
  the dispatch? One line, no surrounding structure, trivially lost in a refactor.
- Are the hook insertion points in `Call()` and `create()` intact, each gated by
  `evm.deployerProxyHooksActive()`?
- Do the two `create()` sites still branch on `HasDeploymentHookFix` for *placement* (pre- vs
  post-collision-check)? That rule is a timing selector, not a gate.
- Is `deployerProxyHooksActive()` (`HasRuntimeUpgrade && !DeployerProxySunset`) still the only
  gate, with no hardcoded fork check reintroduced at a call site?
- Does `RunPrecompiledContract()` still copy the input buffer for zero-gas contracts?
- Is deployer attribution still `tx.Origin` vs direct caller per the `HasDeployOrigin` /
  `DeployerFactory` rules?

**The hooks are never deleted.** They are inert after `DeployerProxySunset`, but every historical
block in the `RuntimeUpgrade → DeployerProxySunset` window ran them, and replaying that range needs
the identical path. The predicate turns the behaviour off; deletion breaks history.

## 2. Parlia: tokenomics, inflation and Chiliz hardforks

**Files:** `consensus/parlia/`, `consensus/misc/eip1559/`, `consensus/consensus.go`

The highest-risk area in the repository. `distributeIncoming()` is the most conflict-prone single
function — Dragon8/Pepper8/Pipe8 branching layered on BSC's reward logic.

**Dragon8 — per-block inflation.** `getInflationPct()` (exponential decay
`9.24·e^(−0.25·year)+1.60`, floored at 1.88% after year 13), `getNewSupplyForBlock()`,
`getNewSupplyForBlockDragon8Fix()` (hardcoded 14-year schedule), and `distributeToTokenomics()`,
which deposits the mint plus gas fees into the Tokenomics contract (`0x…7006`) via
`deposit(validator, newTotalSupply, inflationPct)`. When Dragon8 is active, BSC's system-reward
distribution is skipped entirely.

- Does `getLastSupplyFromTokenomics()` still resolve with
  `BlockNumberOrHashWithHash(header.ParentHash, false)` — never by number? (See COR-184 in
  `regressions.md`; this is the only by-number state read that ever existed in consensus.)
- Is it still confined to the `else if IsDragon8(...)` branch, and still hard-fail there?

**Pepper8 / Pipe8 — one-time mints.** `pepper8Fork.go`, `pipe8Fork.go`, `common/pepper8/`,
`common/pipe8/`. At each fork's activation block a fixed amount (148.6M CHZ for Pepper8, ~455.7M
for Pipe8) is minted to the coinbase and forwarded by a zero-gas-price system transaction to
`0xE0d17A41C1A4Fe527e375C644F9D2A02e96111ED`. Pepper8 also deploys the deterministic deployment
proxy at `0x4e59b44847b379578588920cA78FbF26c0B4956C`.

**Snake8 — validator frequency data in headers.** Adds a `"VFQ"`-prefixed section (parent timestamp
+ RLP frequency data) to header `Extra`, rewrites `getValidatorBytesFromHeader()`, extends
`snapshot()` to `(…, isSnake8Fork, extraHeader)` across 25+ call sites, adds `IsSnake8Fork` and
`FrequencyRLP` to `Snapshot`, and forces `TurnLength = 50`.

- **Ordering:** does `refreshFrequencyRLP()` still run *before* `header.Difficulty = calcDifficulty(...)`
  and before the Ramanujan backoff in `prepare()`, with `SetExtraData` embedding the same bytes?
- Is difficulty still computed for `header.Coinbase` rather than `p.val`?

**Snake8Fix (COR-173).** Post-`Snake8FixTime` (gated on **parent**.Time): producers pin stake reads
to the parent block's state, a sealer that cannot read stakes refuses to seal, and verifiers
recompute the bytes in `Finalize` → `verifySnake8FrequencyData()`.

- Is the verification still in `Finalize`, not `verifyCascadingFields`?
- Does it still **fail open** on `errSnake8StakeLookup` and condemn only on byte mismatch, while
  the sealer stays hard-fail?
- Is the pre-fork latest-state path still uncached and still un-tightened?

**Other pins.** The bid-block `signableSystemTxSelectors` assertion in `New()` stays wrapped in
`PlatoBlock != nil && FeynmanTime != nil`; `systemRewardPercent = 5`; `defaultEpochLength` is a
`var`; `isToSystemContract()` uses the shared registry plus the Pepper8/Pipe8 recipients; the
`consensus.PoSA` interface carries `IsPepper8Deposit/Block`, `GetPepper8MintAmount`,
`IsPipe8Deposit/Block`, `GetPipe8MintAmount`, `IsTokenomicsDeposit`; EIP-1559 clamps the base fee at
`InitialBaseFeeForBSC` rather than letting it decay to zero; +1s tolerance on the future-block-time
check.

## 3. Gas and fee rules

**Files:** `params/protocol_params.go`, `core/state_transition.go`, `eth/gasprice/gasprice.go`

- Chiliz values live in `ForBSC`-suffixed constants; the unsuffixed ones keep upstream defaults for
  non-Parlia chains, selected at the call site by the Parlia test in any of its three equivalent
  spellings: `config.Parlia != nil`, `IsInBSC()` / `IsNotInBSC()`, or `MinBlobGasprice(config)`.
  `InitialBaseFeeForBSC` = 2,500 Gwei vs 1 Gwei; `BlobTxMinBlobGaspriceForBSC` = 150 Gwei vs 1 wei.
  **Using the unsuffixed name on a Parlia path compiles and is wrong by 2500x.**
- Zero-gas-price system transactions and Pepper8/Pipe8 coinbase deposits are exempt from
  `GasFeeCap >= BaseFee`, with the effective tip clamped at 0.
- `DefaultMaxPrice = 20000 GWei`.

## 4. Chain config and networks

**Files:** `params/config.go`, `config/embeded.go`, `config/embedded/*.json`, `params/bootnodes.go`,
`version/version.go`, `params/version.go`, `cmd/utils/flags.go`, `cmd/geth/config.go`

- Chiliz fork fields, their `Is<Fork>()` helpers and their `Rules` flags must all be present
  together — a rule flag declared but never set reads as "never activates".
- Fork **activation** in `config/embedded/*.json` is a business decision. A new BSC hardfork
  arriving via a sync gets its fields in `params/config.go` and nothing in the genesis JSON.
- Both version files carry the Chiliz `2.x.y` numbers, agree with each other, and are bumped on
  release-bound PRs.
- `--chiliz` / `--spicy` / `--scoville` / `--genesis` flags and the embedded-genesis loading logic
  are ours.

## 5. Replay and tracing consistency

**Files:** `eth/tracers/api.go`, `eth/state_accessor.go`, `eth/tracers/native/prestate.go`,
`eth/tracers/js/internal/tracers/prestate_tracer.js`, `internal/replay/`

The custom mints must be mirrored anywhere state is re-executed outside consensus, or traces and
state diverge. **Any new replay path — tracer API, simulation endpoint, state-reconstruction
helper — needs the same `IsTokenomicsDeposit()` / `IsPepper8Deposit()` / `IsPipe8Deposit()`
handling.** This is how a cleanly-merged upstream feature breaks Chiliz silently.

## 6. Smaller behavioural deltas

`eth/sync.go` `defaultMinSyncPeers = 1`; the exported `StateObject` alias and
`GetOrNewStateObject()` in `core/state/`; the public `EffectiveGasPrice()` helper on
`core/types/transaction.go`.

`p2p/dnsdisc/sync.go` carries a Chiliz `len(ct.links.missing) == 0` check in `syncNextLink`, but it
sits *after* the `ct.links.missing[0]` it would protect, and the only caller reaches it only when
`!ct.links.done()` holds — `done()` is `len(missing) == 0`, so the slice is non-empty by
construction there and the check is unreachable, guarding nothing. Treat
it as harmless dead code, **not** as an invariant: do not block an upstream cleanup that removes
it, and do not accept a "restoration" of it in the same position. Making it a real guard means
moving it above the index, which is a code change, not a review note.

## Known fragility — flag, do not fix opportunistically

`Parlia.snapshot()` mutates shared LRU entries in place with caller-dependent `extraHeader` data
(COR-174 addressed the copy semantics). An upstream refactor of `snapshot()` or its caching
interacts with this. Raise it for human review rather than proposing a fix in passing.
