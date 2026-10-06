---
applyTo: "core/vm/evm.go,core/vm/chiliz.go,core/vm/contracts.go,core/vm/interpreter.go,core/vm/errors.go,core/vm/systemcontract/**,common/systemcontract/**,core/state/**"
---

# Reviewing the Chiliz EVM hooks

Chiliz intercepts contract deployment and invocation in the EVM and delegates permission checks to
the **DeployerProxy** system contract (`0x…7005`). Upstream refactors of the call/create path touch
this constantly, and the hooks are small enough to disappear without a conflict.

## What must survive any refactor

- Every `evm.precompile(addr)` call upstream is replaced by `evm.precompileOrHook(addr, caller)` in
  `Call`, `CallCode`, `DelegateCall` and `StaticCall`. Flag any dispatch site that reverts to the
  bare `precompile()`.
- `Call` carries a bare `evm.warmDeployerProxyOnHookDispatch(addr)` line immediately after the
  `precompileOrHook` dispatch (COR-193). It is **one line with no surrounding structure** — exactly
  the shape an upstream call-path refactor drops silently. Losing it makes mainnet block 31,384,697
  and spicy 32,196,267 unimportable again, i.e. a full resync stops dead. Flag its absence.
- Hook insertion points exist in `Call()` and `create()`. The two `create()` sites branch on the
  `HasDeploymentHookFix` rule — that is a **timing selector** (pre- vs post-collision-check
  placement), not an on/off gate. Do not let it be collapsed into the gate.
- `hookStateDBAdapter` wraps StateDB for hook calls.
- `RunPrecompiledContract()` in `contracts.go` copies the input buffer for zero-gas contracts —
  the EVM hooks consume 0 gas and rely on this.

## The gate

`deployerProxyHooksActive()` in `core/vm/chiliz.go` is the **single** predicate gating both hooks:
`HasRuntimeUpgrade && !DeployerProxySunset`. Flag any hardcoded `HasRuntimeUpgrade` or sunset check
reintroduced at a call site by a merge — the predicate is the only gate.

**The hooks are never deleted, even though they are inert after the sunset fork.** Mainnet and
spicy are live chains: every historical block produced in the `RuntimeUpgrade → DeployerProxySunset`
window executed these hooks, so re-syncing that range requires the identical code path. The
predicate is what turns the behaviour off for new blocks; deletion would be a consensus break on
history.

## Deployer attribution

`registerDeployedContract` is passed `tx.Origin` or the direct caller depending on the
`HasDeployOrigin` / `DeployerFactory` rules. A change to which one is passed changes recorded
deployer identity — fork-gated or it is a break.

## Runtime upgrade hook

`core/vm/systemcontract/upgrade.go`: the `RuntimeUpgradeContract` (`0x…7004`) replaces system
contract bytecode in place via `upgradeTo(address,bytes)` at hook address `0x…7f01`, gated on
`HasRuntimeUpgrade`. It depends on the exported `StateObject` alias and `GetOrNewStateObject()` in
`core/state/`.

## System contracts

`common/systemcontract` holds the address registry (`IsSystemContract()`), the `IEvmHooks` ABI and
the constants. BAS system contracts live at `0x…7001`–`0x…7006` (staking, governance, chain config,
runtime upgrade, deployer proxy, tokenomics) and **bypass all hooks**.

## Watch for un-fork-gated upstream checks

Upstream occasionally adds validity checks with no fork gate — `ErrCoinbaseAsContract` in
`interpreter.go` was one. On a Parlia chain these retroactively change replay semantics. Any new
unconditional rejection or gas change in this directory needs a fork gate or an argument for why
it cannot affect historical blocks.
