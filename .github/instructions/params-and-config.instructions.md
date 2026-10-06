---
applyTo: "params/**,config/**,core/genesis.go,eth/gasprice/**"
---

# Reviewing chain params and network config

## The `ForBSC` trap

Chiliz values live in **`ForBSC`-suffixed** constants in `params/protocol_params.go`; the
unsuffixed names keep upstream's defaults for non-Parlia chains, and the call site selects between
them on whether the chain is Parlia. That test is spelled three ways in production, all equivalent
and all correct: `config.Parlia != nil` (used directly in `consensus/misc/eip1559`); the helpers
`IsInBSC()` / `IsNotInBSC()`, **defined in `params/config.go` as exactly `c.Parlia != nil`** and
called from `core/genesis.go` among others; and the wrapper `MinBlobGasprice(config)`
(`consensus/misc/eip4844`). Do not flag one spelling in favour of another.

- `InitialBaseFeeForBSC = 2_500_000_000_000` (2,500 Gwei) vs `InitialBaseFee` = 1 Gwei.
- `BlobTxMinBlobGaspriceForBSC = 150_000_000_000` vs `BlobTxMinBlobGasprice` = 1 wei.

Reaching for the unsuffixed name on a Parlia code path **compiles cleanly and is silently wrong**:
2500x for the base fee, and 1.5e11x for the blob gas price (1 wei vs 150 Gwei) — quote the right
factor, the blob one is not a rounding error.
Flag every use of an unsuffixed constant in code that can run on a Parlia chain, including tests
and fuzz seeds (COR-214 seeded a corpus that way). Blob retention divisors are also Chiliz-specific
(0.45 → 3, roughly six days of retention).

`eth/gasprice/gasprice.go` carries `DefaultMaxPrice = 20000 GWei`, also a Chiliz value.

## Fork plumbing

Chiliz forks on `ChainConfig` are block-based (`RuntimeUpgradeBlock`, `DeployOriginBlock`,
`DeploymentHookFixBlock`, `DeployerFactoryBlock`) and timestamp-based (`Dragon8Time`,
`Dragon8FixTime`, `Snake8Time`, `Snake8FixTime`, `Pepper8Time`, `Pipe8Time`,
`DeployerProxySunsetTime`). A new fork field needs all of: the field, its `Is<Fork>()` helper, the
flag on the `Rules` struct, and wiring in `Rules()`. Flag a partial addition — a rule flag that is
declared but never set is silently always-false, which reads as "fork never activates".

## Genesis and networks

- `config/embedded/*.json` are the compile-time-embedded genesis configs for chiliz (88888),
  spicy (88882) and scoville (88880), loaded through `config/embeded.go`.
- **Scheduling a fork in these files is a business decision.** Flag any PR that adds or changes a
  fork activation time or block in `config/embedded/*.json` unless the PR explicitly states it was
  asked for. This applies especially to new BSC hardforks arriving through an upstream sync: the
  fork fields belong in `params/config.go`, the activation does not belong in the genesis JSON.
- Changing an existing, already-passed activation time rewrites history. Always a finding.

## Version files

`version/version.go` and `params/version.go` both carry the Chiliz `2.x.y` numbers and must agree.
Never take upstream's numbers. See `version-bump.instructions.md`.
