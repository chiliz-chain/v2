---
applyTo: "consensus/parlia/*_test.go,consensus/parlia/**/*_test.go,consensus/misc/**/*_test.go,core/vm/*_test.go,core/vm/**/*_test.go,common/systemcontract/**/*_test.go,params/*_test.go,config/*_test.go,internal/replay/*_test.go,eth/tracers/*_test.go,eth/tracers/**/*_test.go"
---

# Reviewing tests

## Tripwire tests are never "fixed" by editing the test

Two tests exist specifically to fail when an invariant is disturbed. A diff that changes their
expectations to make them pass is inverting their purpose — that is the finding, and the real fix
is in the production code.

- `TestPrepareDifficultyMatchesEmbeddedFrequencyData` (`consensus/parlia`) pins the Snake8
  producer-coherence ordering: `refreshFrequencyRLP()` before difficulty stamping. It exists
  because a chain split (COR-37) shipped through a green test suite.
- `TestReplayFixtures` (`internal/replay`) re-executes real historical mainnet/spicy transactions
  and pins the gas the chain charged. Its `testdata` values are chain history. Adding fixtures is
  fine; retuning them is not.

The `snake8fix_test.go` suite similarly pins producer pinning, verifier rejection, the pre-fork
skip, sealer refusal and deterministic zero-stake degradation.

## The COR-39 skip pattern

Some upstream tests cannot pass against the Chiliz BAS ABI: those that pack Feynman-only
validator-set methods (`distributeFinalityReward`, `updateValidatorSetV2`) or construct engines
from the fully-forked `ParliaTestChainConfig` for bid-block. They carry a `t.Skip` with a COR-39
comment — `TestParlia_applyTransactionTracing`, `TestParlia_applyTransactionModes`,
`TestParliaFinalizeAndAssembleBidBlock*`, `TestParliaPrepareForBidBlock`.

When an upstream merge brings a new test of that shape, skip it the same way. **Never resolve it
by adding the missing methods to the ABI.**

## Fuzz targets

`run-fuzz.sh` discovers and runs every `Fuzz*` function in the configured packages, so a new target
is fuzzed whether or not it appears in `.github/scripts/fuzz/expected-targets.txt`. That file is a
**per-package minimum-count ratchet**, there so that discovery breaking (targets renamed away from
`Fuzz*`, a package dropped from `fuzz.yml`) is never mistaken for a clean campaign. A package
carrying *more* targets than recorded emits a notice asking for a refresh, not a failure — **never
block a fuzz PR on it**. It must be refreshed in the same PR only when the package minimum is
intentionally raised, or when a package is deliberately removed:

```bash
.github/scripts/fuzz/run-fuzz.sh --write-baseline ./pkg...
```

Seed corpora and harnesses run on Parlia chain configs,
so the `ForBSC` constant rule applies to test code too — a seed built from `InitialBaseFee` instead
of `InitialBaseFeeForBSC` is off by 2500x and will quietly explore the wrong space.

A reproducer for a known-but-unfixed finding is checked in as a skipped test naming its issue,
rather than left out.

## Shared global config

Tests must not mutate shared package-level chain configs (`AllEthashProtocolChanges` and friends).
It creates order-dependent failures that only show up in CI.
