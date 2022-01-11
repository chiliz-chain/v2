Chiliz Genesis
==============

Blockchain Native Address:
- 0x0000000000000000000000000000000000000000

Smart Contract Addresses:
- Deployer: 0x0000000000000000000000000000000000000010
- Governance: 0x0000000000000000000000000000000000000020
- Parlia: 0x0000000000000000000000000000000000000030

### Testnet

How to compile contracts

```bash
cd smartcontract
yarn install
yarn compile
```

How to generate genesis files

```bash
cd config
yarn install
node create-genesis.js
```

How to create validator private key

```bash
geth.exe account new --datadir ./tmp/testnet
```

How to init validator

```bash
geth.exe --datadir ./tmp/testnet init ../chiliz-genesis/config/testnet/genesis.json
```

How to run new validator

```bash
geth.exe --config ../chiliz-genesis/config/testnet/config.toml --datadir ./tmp/testnet -unlock <<your-validator-address>> --password ../chiliz-genesis/config/testnet/password.txt --mine --gcmode archive --allow-insecure-unlock --pprofaddr 0.0.0.0 --metrics --pprof
```

How to run faucet

```bash
--genesis ../chiliz-genesis/config/testnet/genesis.json --bootnodes enode://c31ec52a8c76cdbae847d7d326b7d9591d5e20cf260149013b52f958356055afef1a602fcac7c292e36572db71581cba2bf996edddf0076512c5afec5e804031@127.0.0.1:30311 --account.json ./tmp/testnet/keystore/UTC--2021-04-15T12-00-01.560260400Z--00a601f45688dba8a070722073b015277cf36725 --account.pass ../chiliz-genesis/config/testnet/password.txt
```

How to run simple node

```bash
geth.exe --config ../chiliz-genesis/config/testnet/config.toml --datadir ./tmp/testnet --gcmode archive
```