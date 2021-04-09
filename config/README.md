How to run node
===============


### Testnet

How to run new validator

```bash
make geth
./build/bin/geth --datadir ~/.chiliz/testnet init ./config/testnet/genesis.json
./build/bin/geth account new --datadir ~/.chiliz/testnet
echo 12345 > ~/.chiliz/testnet/password.txt
./build/bin/geth --config ./config/testnet/config.toml --datadir ~/.chiliz/testnet -unlock 0xffFfCFaC046F0EF39D132cEACF108B6eEd6B9cE8 --password ~/.chiliz/testnet/password.txt  --mine --gcmode archive --allow-insecure-unlock --pprofaddr 0.0.0.0 --metrics --pprof
```

```bash
./geth.exe --datadir ./tmp/testnet init ./config/testnet/genesis.json
./geth.exe account new --datadir ./tmp/testnet
./geth.exe --config ./config/testnet/config.toml --datadir ./tmp/testnet -unlock 0x491665d9B0D5a539Da5a3498fDD8427fdc55e93e --password ./config/testnet/password.txt --mine --gcmode archive --allow-insecure-unlock --pprofaddr 0.0.0.0 --metrics --pprof
```