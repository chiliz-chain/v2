#!/usr/bin/env bash

echo "Genesis file is: ${GENESIS_FILE}"
echo "Node mode is: ${NODE_MODE}"

function run_bootnode {
  geth --datadir=/datadir \
    --networkid=17243 \
    --miner.gastarget=30000000 \
    --miner.gaslimit=40000000 \
    --miner.gasprice=5000000000 \
    --nodekeyhex=633ab917d09441de38ae9251e79ced41df39a1c338842b826c18fb1773246e18 \
    --http \
    --http.port=8545
}

function run_validator {
  geth --datadir=/datadir \
    --mine \
    --password=/datadir/password.txt \
    --allow-insecure-unlock \
    --unlock="${VALIDATOR_ADDRESS}" \
    --miner.etherbase="${VALIDATOR_ADDRESS}" \
    --bootnodes=${BOOT_NODES} \
    --gcmode=archive \
    --networkid=17243 \
    --miner.gastarget=30000000 \
    --miner.gaslimit=40000000 \
    --miner.gasprice=5000000000 \
    --nodekeyhex=${NODE_KEY} \
    --http \
    --http.addr=0.0.0.0 \
    --http.port=8545 \
    --http.corsdomain=*
}

echo "12345678" > /datadir/password.txt
geth --datadir=/datadir init "${GENESIS_FILE}"

if [ "${NODE_MODE}" = "bootnode" ]; then
  run_bootnode
elif [ "${NODE_MODE}" = "validator" ]; then
  run_validator
fi

exit