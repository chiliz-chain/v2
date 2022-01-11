#!/bin/bash
cd smartcontract
yarn install
yarn run compile
cd ../config
yarn install
node create-genesis.js