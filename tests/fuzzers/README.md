## Fuzzers

Two kinds of fuzz targets live in this repository:

- **Upstream targets** inherited from go-ethereum/BSC, most of them under this
  directory (`bls12381`, `bn256`, `difficulty`, `rangeproof`, `secp256k1`,
  `txfetcher`) and a few next to the code they test.
- **Chiliz targets** covering the fork delta (COR-207), written as native Go fuzz
  functions next to the code they test: header `Extra` parsing and frequency
  data in `consensus/parlia`, system-transaction classification, the
  upstream-divergence oracles in `consensus/parlia`, `consensus/misc/eip1559`,
  `consensus/misc/eip4844` and `core/vm`, inflation arithmetic, fork rules in
  `params`/`config`, the EVM hooks in `core/vm`, `core/vm/runtime` and
  `core/vm/systemcontract`.

All of them use the standard library's native fuzzing (`testing.F`); the
`go-fuzz` tool is no longer needed.

### Running

List the targets of a package:

```
go test -list '^Fuzz' ./consensus/parlia/
```

Run one target as a plain unit test (its seed corpus only), which is what CI
does on every pull request:

```
go test ./consensus/parlia/ -run '^FuzzHeaderExtraLayout$'
```

Fuzz it for a while (one package per invocation is a Go toolchain limit):

```
go test ./consensus/parlia/ -run '^$' -fuzz '^FuzzHeaderExtraLayout$' -fuzztime 5m
```

A failing input is written under `<pkg>/testdata/fuzz/<Target>/<hash>` and is
replayed automatically by the plain `go test` run above. Once the cause is fixed,
commit that file: it becomes a permanent regression seed.

### Writing a target: the seed corpus is what CI runs

`unit-test.yml` runs `make test`, i.e. plain `go test`. For a fuzz target that
executes **only the seed corpus** — the mutator never runs in CI. The weekly
campaign is the only place `-fuzz` happens. So a property is worth exactly as
much as the seeds that distinguish it, and "the fuzzer found it in 0.08s" is not
the same as "CI would catch it".

Two real examples from the COR-207 review round, both of which ran green in CI
while the property was violated:

- A fork-rules target gave three different block forks the same activation
  height in every seed. Swapping two of them in `Rules()` — an easy upstream
  merge slip — passed the whole seed corpus.
- A snapshot target had no seed carrying a validator-set change, so the
  set-switch branch it existed to cover was never executed by CI at all.

When you add a target, ask of each property: *is there a seed for which this
property, and only this property, distinguishes correct from broken?* Give each
fork, flag and code path a seed where it differs from its neighbours, and prove
it by mutating the production code and running plain `go test` — not `-fuzz`.
If only `-fuzz` catches your mutation, the seed corpus has a hole.

Fuzzer-found crashers become permanent seeds, which is the other half of this:
commit the `testdata/fuzz/...` file so the case is replayed on every PR forever.

### The weekly campaign

`.github/workflows/fuzz.yml` runs every target in the Chiliz packages for a
few minutes each on a schedule (and on `workflow_dispatch`, where the budget and
the package list can be overridden). Targets are discovered with `go test -list`,
so a new `Fuzz*` function in one of those packages is enrolled automatically. A
crasher fails the run and the input is uploaded as an artifact.
