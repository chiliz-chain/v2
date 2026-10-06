---
name: code-review
description: Chiliz Chain (CC2) consensus-safety review checks for pull requests. Use when reviewing any change to consensus (consensus/parlia, consensus/misc), EVM execution (core/vm, core/vm/systemcontract, common/systemcontract), state and transaction semantics (core/state, core/state_transition.go, core/genesis.go, core/types/transaction.go), chain parameters or genesis config (params, config/embedded), transaction replay and tracing (eth/tracers, eth/state_accessor.go, internal/replay), or any pull request that merges an upstream BNB Smart Chain tag. Supplies the fork invariants that must not be broken, the shapes of past consensus-breaking regressions, and how to review an upstream sync merge.
---

# Reviewing pull requests on Chiliz Chain v2

This repository is the client for a **live Layer 1** — mainnet (chain ID 88888) and spicy testnet
(88882) both carry real value — and it is a fork of `bnb-chain/bsc`, itself a fork of go-ethereum.
Most of the tree is inherited upstream code. A small Chiliz delta sits inside it, and that delta is
where reviews pay for themselves.

## The two questions

1. **Does this change what a node computes for a block it has already seen?** Every block the
   fleet produced must still re-execute identically on a resync. Gas, state access, header
   parsing, reward distribution, EVM dispatch: change any of them without a fork gate and a resync
   stops dead at a block the chain already accepted. Unit tests pass right up until it does.
2. **Did this change preserve every line but reorder two of them?** Consensus code is
   order-sensitive, and our worst outage came from a resolution in which every Chiliz line survived
   intact and two of them swapped places. Presence is not correctness.

## How to work through a PR

**First, classify it** — the review surface differs sharply:

| Kind | How to tell | Surface |
|---|---|---|
| Upstream BSC sync | branch/title mentions a `v1.x.y` BSC tag, huge diff, `.github/bsc-sync/last-synced-tag` moves | `upstream-sync-review.md` — conflict resolutions and clean-merge blast radius only |
| Chiliz change | `feat(COR-x)` / `fix(COR-x)`, touches the delta | `invariants.md` for the areas touched |
| CI / tooling | `.github/**` only | the CI instructions; treat as production automation |
| Test / fuzz | `*_test.go` only | the test instructions; watch for tripwires being retuned |

**Then, for anything touching execution**, walk `invariants.md` for the areas the diff reaches and
check `regressions.md` for a matching shape. The regressions file is the more valuable of the two:
each entry describes a *class* of mistake, not just the instance, and every one of them shipped
past a green test suite.

**Report at the right altitude.** A consensus break is worth a blocking comment with the block
numbers or fork window it affects. Do not bury it under style notes on upstream code — see the
"Do not flag" list in `.github/copilot-instructions.md`.

## Files here

- `invariants.md` — the Chiliz-specific behaviour that must not be lost, as review questions.
- `regressions.md` — how we have actually broken this chain, written as patterns to match.
- `upstream-sync-review.md` — reviewing a merge of an upstream BSC tag.

`CLAUDE.md` in the repository root is the fuller merge runbook. It is written for the agent doing
the merge rather than for a reviewer, so use it as background.
