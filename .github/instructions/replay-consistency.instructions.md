---
applyTo: "eth/tracers/**,eth/state_accessor.go,core/state_transition.go,core/types/transaction.go,internal/replay/**,cmd/replaycheck/**"
---

# Reviewing transaction replay and tracing

Chiliz mints CHZ in places upstream does not: the Tokenomics deposit (Dragon8, every block), and
the one-time Pepper8 and Pipe8 mints. Anywhere state is re-executed **outside** consensus, those
credits have to be mirrored or traces and state diverge from the chain.

## The mirroring rule

Existing sites, all of which check `IsTokenomicsDeposit()` / `IsPepper8Deposit()` /
`IsPipe8Deposit()` after system transactions and credit the coinbase:

- `eth/tracers/api.go`
- `eth/state_accessor.go`
- `eth/tracers/native/prestate.go` and `eth/tracers/js/internal/tracers/prestate_tracer.js`

**If a PR adds a new code path that replays transactions — a new tracer API, a simulation
endpoint, an `eth_simulateV1`-style feature, a new state-reconstruction helper — the deposit
handling must be added there too.** This is the most likely way an upstream feature breaks Chiliz
silently: it merges cleanly, it is correct for BSC, and it reports wrong balances here. Flag a new
replay path that does not handle the deposits.

Upstream changes to tracer internals that do *not* touch these blocks are fine to take as-is.

## Fee validation

`core/state_transition.go` exempts zero-gas-price system transactions and Pepper8/Pipe8 deposits
from the coinbase from the `GasFeeCap >= BaseFee` check, and clamps the effective tip at 0 to avoid
negative-tip arithmetic. Flag any tightening of these exemptions, and flag a switch of the
system-transaction detection predicate (for example to an `EffectiveGasPriceForBSC()`-based check)
that is not fork-gated — it retroactively changes which historical transactions counted as system
transactions.

`core/types/transaction.go` carries the public `EffectiveGasPrice()` helper (and
`EffectiveGasPriceForBSC()`) that this detection path depends on — a Chiliz export, not upstream's.

## Replay fixtures

`internal/replay/TestReplayFixtures` re-executes real historical mainnet and spicy transactions
offline against their captured pre-state and pins the gas the chain actually charged. It is the
tripwire for any un-fork-gated change that alters gas retroactively — the COR-193 class, where a
resync stops dead at a block the fleet already produced.

**A diff that edits expected values in `internal/replay/testdata` is a finding, not a fix.** Those
numbers are chain history. If the fixture fails, the change altered replay semantics; the change
is what needs fixing. Adding *new* fixtures is fine.
