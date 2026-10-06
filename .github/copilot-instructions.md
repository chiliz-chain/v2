# Copilot instructions — Chiliz Chain v2

## What this repository is

The chain client for **Chiliz Chain** (CC2), an EVM-compatible Layer 1 whose native token is CHZ.
It is a fork of [bnb-chain/bsc](https://github.com/bnb-chain/bsc), itself a fork of go-ethereum,
and we periodically merge upstream BSC tags. Two networks running on it are live and carry real
value: **mainnet** (chain ID 88888) and **spicy** (testnet, 88882).

Almost every file here is inherited upstream code. A small delta is Chiliz's, and that delta is
where the risk lives.

## The rule that governs every review

**A change that alters execution semantics without a fork gate is a consensus break on historical
blocks.** Every block the fleet has already produced must still re-execute to the same result
when a node re-syncs. A change to gas accounting, state access, header parsing, reward
distribution or EVM dispatch that is not gated behind a fork rule will stop a resync dead at a
block the chain already accepted — and unit tests pass right up until it does.

So the first question on any diff touching execution is: *does this change what a node computes
for a block it has already seen, and if so, is it behind a fork gate?*

The second question, which has cost us more than the first: *did this change preserve every line
but reorder two of them?* Consensus code is order-sensitive. See
`.github/skills/code-review/regressions.md`.

## Where the detail lives

- **`.github/skills/code-review/`** — the consensus-safety checklists, the catalogue of
  invariants, and the regression shapes worth pattern-matching. Load it for any change to
  `consensus/`, `core/`, `params/` or `eth/tracers/`, and for any upstream BSC sync PR.
- **`.github/instructions/*.instructions.md`** — per-area checks, applied automatically by path.
- **`CLAUDE.md`** — the upstream-merge runbook. Written for the merge, not for the review; use it
  as background, not as a review checklist.

## Do not flag

These produce noise on this repo specifically:

- **Inherited upstream code the PR only moves, re-indents or merges through.** An upstream sync
  PR carries thousands of lines of go-ethereum/BSC changes we did not write and do not review
  line by line. Review the conflict resolutions and the Chiliz delta.
- **Style, naming or idiom suggestions on upstream files.** We keep them diffable against
  upstream on purpose; gratuitous divergence costs us at every merge.
- **Suggestions to remove "unused"-looking Chiliz code in a fork window.** Do not propose the
  removal, and **do flag a diff that performs one** — but this covers *fork-window execution code*
  only: the DeployerProxy EVM hooks in `core/vm/`, the pre-Dragon8Fix supply read, and their kin.
  They look dead once `DeployerProxySunset` is active. They are not: every historical block in the
  `RuntimeUpgrade → DeployerProxySunset` window executed them, so replaying that range needs the
  exact same code path, and catching that deletion is one of the highest-value findings available
  here. It is **not** a blanket rule against deleting Chiliz-authored code — genuinely unreachable
  code that never affected a block (see the `p2p/dnsdisc` note in
  `.github/skills/code-review/invariants.md`) can be cleaned up freely.
- **Adding missing methods to the BAS system-contract ABIs.** When an assertion or a test fails
  because `consensus/parlia/abi.go` lacks a method upstream expects (`distributeFinalityReward`,
  `updateValidatorSetV2`), the fix is to fork-gate or skip, never to extend the ABI. Chiliz's
  system contracts genuinely do not have those methods.
- **Suggestions to "fix" a failing tripwire test by editing the test.** See
  `.github/instructions/go-tests.instructions.md`.

## The one code-level rule that is always on

**`ForBSC`-suffixed constants.** Chiliz fee values live in `ForBSC`-suffixed constants in
`params/protocol_params.go`; the unsuffixed names hold upstream's defaults for non-Parlia chains.
Using an unsuffixed constant on a code path that can run on a Parlia chain **compiles cleanly and
is silently wrong** — 2500x for `InitialBaseFee` (1 vs 2,500 Gwei), 1.5e11x for
`BlobTxMinBlobGasprice` (1 wei vs 150 Gwei). This one is repeated here, rather than left to
`.github/instructions/params-and-config.instructions.md`, because the mistake lands anywhere the
constants are reachable — `core/`, `eth/`, `miner/`, `cmd/`, and test and fuzz-seed files (COR-214
was a fuzz seed) — not only under `params/`. Flag it wherever you see it.

## Conventions worth checking

- Commits are conventional and scoped to a Linear issue: `fix(COR-193): …`, `ci(COR-243): …`.
  The PR title carries the same `COR-xxx`.
- Any PR that will ship a release must bump the patch component in **both** `version/version.go`
  and `params/version.go`, and the two must agree. The release tag has to equal the compiled-in
  version. **A release-bound PR that touches neither file is the finding** — check for the omission,
  not only for a mismatch between the two.
- Shell in `.github/scripts/` and workflow steps get real scrutiny — this is unattended
  automation that pages people. See `.github/instructions/ci-workflows.instructions.md`.

## Using the Linear issue

PR titles name a `COR-xxx` Linear issue. When the Linear MCP server is available, read it and
review the diff against the stated intent: flag scope drift, and flag acceptance criteria in the
issue that the diff does not appear to satisfy.
