# Reviewing an upstream BSC sync PR

These PRs come from `.github/workflows/bsc-upstream-sync.yml`, which merges upstream BSC tags one
at a time, has an agent resolve each conflict, and opens a **draft** PR with a release-impact
report. They are enormous and almost none of the diff is ours.

## The review surface

**In scope**

1. **Every conflict resolution.** The PR body and the sync report list them with a confidence
   marker (🟢 high / 🟡 low / 🔴 unresolved). Read 🟡 and 🔴 closely; do not assume 🟢 is safe in the
   files listed in `invariants.md`.
2. **Cleanly-merged upstream code that lands in or near the Chiliz delta.** This is where the real
   risk is. The bulk of a release merges without conflict and is the easy thing to miss — PR #51's
   ship blocker was a clean merge (see Shape 5 in `regressions.md`).
3. **New upstream files that simply appeared**, because the merge base never had them. Workflows
   are the recurring case: `.github/workflows/` must contain only the Chiliz-owned list, and
   upstream ones get deleted on every sync.
4. **Version files.** `version/version.go` and `params/version.go` must both stay on the Chiliz
   `2.x.y` numbers, agree, and be bumped if this sync will ship. The PR body carries a **Version**
   note saying whether the script bumped it; trust that note rather than re-bumping.
5. **`CHANGELOG.md`.** By convention each sync prepends the upstream BSC release notes for the
   merged tags above the existing content. Upstream's version of the file must not replace ours.
6. **`.github/bsc-sync/last-synced-tag`** should have advanced to the newly merged tag.

**Out of scope** — do not comment on it

The upstream delta itself: thousands of lines of go-ethereum/BSC changes we did not write, do not
maintain and deliberately keep diffable against upstream. Style, naming and idiom notes on those
files are noise.

## Where to look first

In rough order of how often they have gone wrong:

1. `consensus/parlia/parlia.go` — especially `distributeIncoming()`, `snapshot()`, header `Extra`
   parsing, the system-transaction detection path, and anything that reorders `Prepare` /
   `SetExtraData` / `Seal`.
2. `core/vm/evm.go` — `Call` / `create` / precompile dispatch and the hook insertion points.
3. `core/state_transition.go` and `consensus/misc/eip1559/` — fee validation and base-fee
   calculation.
4. `params/protocol_params.go` and `params/config.go` — the `ForBSC` constants and fork plumbing.
5. `eth/tracers/` and `eth/state_accessor.go` — deposit mirroring, plus any *new* replay path.
6. `consensus/parlia/abi.go` — Chiliz uses different system-contract ABIs in places, plus the
   Tokenomics ABI.

## Take the upstream side by default

General EVM/protocol upgrades (new EIPs, opcode and gas-schedule changes other than the constants
above); performance work in trie, snapshot, freezer, database, txpool and p2p internals; Go
toolchain and `go.mod` bumps; new upstream features that miss the invariant files; tracer internals
outside the deposit blocks.

## Keep ours entirely

`.github/workflows/` and the sync tooling in `.github/scripts/bsc-sync/`; `Dockerfile*`,
`docker-compose*.yaml`, `dc.yaml`, `docker/`, Makefile docker targets, `prometheus.yml`;
`cmd/faucet/` customisations; `README.md`, `HOWTO.md`; `CHANGELOG.md`; the version files.

## Business decisions, not review decisions

A new BSC hardfork must be added to `params/config.go` alongside the Chiliz fork fields, but
**activating it for a Chiliz network in `config/embedded/*.json` is a business decision**. Flag any
activation that the PR does not state was explicitly requested.

## What a green CI does not tell you

The build passing and the tests passing were both true for COR-37 and for PR #51. If the merge
touched `Prepare`, `Seal`, `snapshot()` or worker ordering, the release needs a multi-validator
devnet run (`dc.yaml`, 4 nodes, a few hundred blocks, all heads in lockstep). Say so in the review
if the PR does not mention one.

Known intentional test skips carry a COR-39 comment; a new upstream test of that shape should be
skipped the same way rather than fixed by touching the ABI.
