#!/usr/bin/env bash
yarn compile
node scripts/build_abi.js
node create-genesis.js
cp -r ./build/abi ../core/systemcontracts/abi
