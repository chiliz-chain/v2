# replaycheck — checking that a build can still replay old blocks

## What this is for

Some code changes accidentally change how much gas an old transaction costs. When that
happens, the client can no longer re-execute blocks that are already on the chain, so a
fresh archive sync stops dead at that block and never gets past it. That is what happened
in COR-193.

`replaycheck` catches this before a resync does. You give it a block number, it downloads
that block's history from any RPC endpoint, and then it re-runs every transaction in the
block **using the code you have checked out right now**. If your build charges different
gas than the chain charged, it tells you and exits with an error.

The RPC endpoint is only used to fetch history. It is never asked whether the numbers are
correct — that comes from your local build. This matters: it means you can test a fix
before deploying it anywhere.

## Before you start

You need:

- Go installed (the repo's normal build requirements).
- An RPC endpoint with the `debug` namespace enabled and enough history to reach the block
  you care about. The public ones work: `https://rpc.chiliz.com` (mainnet, the default) and
  `https://spicy-rpc.chiliz.com` (Spicy).

## Step 1 — build it

```
make replaycheck
```

This puts the binary at `./build/bin/replaycheck`. You can also skip the build and use
`go run ./cmd/replaycheck` instead — every example below works either way.

## Step 2 — check a single block

```
./build/bin/replaycheck --block 31384697
```

It prints one line per block and finishes with a verdict. If everything lines up:

```
block 31384697 (requested), 3 tx, baseFee 2500000000000
  match

OK: this build replays all 1 block(s) exactly
```

If your build disagrees with the chain, you get the transaction, both numbers and the
difference:

```
block 31384697 (requested), 3 tx, baseFee 2500000000000
  tx 0 0x82de9da7…: canonical 1450973, this build 1453473 (+2500)
  fee pool from this build's gas 2906946000000000 != system-transaction payout 2901946000000000 (+5000000000000) -> this build cannot import this block

error: this build diverges on 1 of 1 block(s): [31384697]
```

Read it like this:

- **canonical** — what the chain actually charged, from the receipt.
- **this build** — what your checked-out code charges now.
- **+2500** — how far off you are. Anything other than zero is a problem.
- The **fee pool** line is the important one. Every block ends with system transactions
  that pay out the block's fees, and the amount is derived from gas used. If your gas is
  wrong, that payout no longer adds up, and that is exactly the error an importing node
  raises. When you see "cannot import this block", a resync would stop there.

On blocks with no fees to pay out you will see `no deposit() system transaction in this
block` instead of a fee-pool line. That is normal, not a warning — there was nothing to
cross-check. The per-transaction gas comparison still ran.

The command exits `0` when everything matches and non-zero when it does not, so you can use
it in CI or as a `git bisect` predicate.

## Step 3 — check more than one block

A range (inclusive, up to 10,000 blocks):

```
./build/bin/replaycheck --block 31000000-31000010
```

Several blocks or ranges at once — repeat the flag:

```
./build/bin/replaycheck --block 31384697 --block 16993611
```

Every system-contract runtime upgrade the network has ever done:

```
./build/bin/replaycheck --upgrades
```

Every governance proposal that was executed:

```
./build/bin/replaycheck --governance
```

These two are the useful ones after a BSC upstream merge. They find the interesting blocks
themselves by scanning logs, so you do not need to know which blocks to look at. A full
mainnet sweep is around 50 blocks and takes a few minutes.

## Step 4 — check Spicy instead of mainnet

Point `--rpc` at the other endpoint. Everything else is the same:

```
./build/bin/replaycheck --rpc https://spicy-rpc.chiliz.com --upgrades
```

The tool reads the chain ID from the endpoint and picks the matching fork schedule from
`config/embedded/`, so you do not have to tell it which network you are on.

## Step 5 — see what the deployed nodes think too

Add `--remote-trace`:

```
./build/bin/replaycheck --block 31384697 --remote-trace -v
```

Now each line shows a third number: what the endpoint's own node charges. This is how you
tell two different situations apart:

| Your build | The endpoint | What it means |
|---|---|---|
| matches | matches | Nothing wrong. |
| matches | wrong | Your fix works, the deployed nodes still need upgrading. |
| wrong | matches | You broke something the deployed nodes get right. |
| wrong | wrong | A real bug that is live in production. |

`-v` prints every transaction rather than only the failing ones. Useful when you want to
see the numbers rather than just a pass or fail.

## Step 6 — turn a block into a permanent test

If you find a block worth guarding forever, save it as a fixture:

```
./build/bin/replaycheck --block 31384697 --save-fixture internal/replay/testdata
```

That writes one JSON file per transaction containing the transaction, the state it read and
the gas the chain charged. Commit those files. From then on:

```
go test ./internal/replay/
```

replays them offline — no network, no endpoint, no chain state — and fails if any future
change moves the gas. Adding a fixture needs no code changes; the test picks up whatever is
in the directory. Open the file and fill in the `comment` field to say why the block is
there, so whoever hits the failure later knows what it is protecting.

This offline test is part of the smoke set to run after any upstream merge:

```
go test ./consensus/parlia/... ./core/vm/... ./params/... ./internal/replay/...
```

## Things worth knowing

- **A failing fixture test is a real signal, not a flaky one.** If `TestReplayFixtures`
  starts failing after a merge, some change altered gas for blocks already on the chain.
  Find the change. Do not adjust the expected number in the fixture.
- **This does not replace a resync.** It checks the blocks you point it at. Only a full
  sync from genesis proves that no *other* block diverges.
- **System transactions have one caveat.** The tool also replays the block's own system
  transactions (the `deposit()` calls at the end of each block). Their starting state is
  captured from the endpoint's tracer, which sets the coinbase balance up slightly
  differently than block production does. In practice these match, but if one of them
  reports a difference, rule that out before believing it. A normal user transaction has no
  such caveat.
- **Speed.** Each transaction needs two or three RPC calls, so expect a few seconds per
  block against a public endpoint. Transient network failures are retried automatically, and
  a block that cannot be fetched is reported as unchecked rather than killing the whole run.

## All the options

```
--rpc URL              endpoint to fetch history from (default https://rpc.chiliz.com)
--block N | N-M        block or inclusive range to check; repeatable
--upgrades             check every system-contract runtime upgrade on the network
--governance           check every executed governance proposal on the network
--from N / --to N      limit the block range that --upgrades and --governance scan
--chunk N              log-scan chunk size for those sweeps (default 4,000,000)
--genesis NAME         force chiliz, spicy or scoville instead of using the chain ID
--save-fixture DIR     write a replay fixture per transaction into DIR
--remote-trace         also report the gas the endpoint itself charges
-v                     print every transaction, not just the failing ones
```
