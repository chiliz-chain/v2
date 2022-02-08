#!/usr/bin/env bash

ls -al /usr/local/bin
which geth
chmod +x /usr/local/bin/geth

wait_for_host_port() {
    while ! nc -v -z -w 3 ${1} ${2} >/dev/null 2>&1 < /dev/null; do
        echo "$(date) - waiting for ${1}:${2}..."
        sleep 5
    done
}

get_host_ip() {
    local host_ip
    while [ -z ${host_ip} ]; do
        host_ip=$(getent hosts ${1}| awk '{ print $1 ; exit }')
    done
    echo $host_ip
}

echo "Genesis file is: ${GENESIS_FILE}"
echo "Node mode is: ${NODE_MODE}"

function run_bootnode {
  geth --datadir=/datadir \
    --networkid=17243 \
    --miner.gastarget=30000000 \
    --miner.gaslimit=40000000 \
    --miner.gasprice=5000000000 \
    --nodekeyhex=633ab917d09441de38ae9251e79ced41df39a1c338842b826c18fb1773246e18 \
    --syncmode=full \
    --http \
    --http.addr=0.0.0.0 \
    --http.port=8545 \
    --http.corsdomain=*
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
    --syncmode=full \
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

sleep ${SLEEP_FOR:-0}

echo "12345678" > /datadir/password.txt
geth --datadir=/datadir init "${GENESIS_FILE}"

if [ "${NODE_MODE}" = "bootnode" ]; then
  run_bootnode
elif [ "${NODE_MODE}" = "validator" ]; then
  run_validator
fi

exit