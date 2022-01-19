const fs = require('fs');
const eth = require('ethereumjs-util');
const BigNumber = require('bignumber.js');
const utils = require('web3-utils');

const GENESIS_CONFIG = {
  "config": {
    "chainId": 0,
    "homesteadBlock": 0,
    "eip150Block": 0,
    "eip150Hash": "0x0000000000000000000000000000000000000000000000000000000000000000",
    "eip155Block": 0,
    "eip158Block": 0,
    "byzantiumBlock": 0,
    "constantinopleBlock": 0,
    "petersburgBlock": 0,
    "istanbulBlock": 0,
    "muirGlacierBlock": 0,
    "parlia": {
      "period": 12,
      "epoch": 10,
    }
  },
  "nonce": "0x0",
  "timestamp": "0x5e9da7ce",
  "extraData": "0x00",
  "gasLimit": "0x2625a00",
  "difficulty": "0x1",
  "mixHash": "0x0000000000000000000000000000000000000000000000000000000000000000",
  "coinbase": "0x0000000000000000000000000000000000000000",
  "alloc": {
    "0xffffFFFfFFffffffffffffffFfFFFfffFFFfFFfE": {
      "balance": "0x0"
    },
    "0x86d274133714A88CE821F279e5eD3fb0BfB42503": {
      "balance": "0x21e19e0c9bab2400000"
    },
    "0x00a601f45688dba8a070722073b015277cf36725": {
      "balance": "0x21e19e0c9bab2400000"
    },
    "0xbAdCab1E02FB68dDD8BBB0A45Cc23aBb60e174C8": {
      "balance": "0x21e19e0c9bab2400000"
    },
    "0x57BA24bE2cF17400f37dB3566e839bfA6A2d018a": {
      "balance": "0x21e19e0c9bab2400000"
    },
    "0xEbCf9D06cf9333706E61213F17A795B2F7c55F1b": {
      "balance": "0x21e19e0c9bab2400000"
    },
  },
  "number": "0x0",
  "gasUsed": "0x0",
  "parentHash": "0x0000000000000000000000000000000000000000000000000000000000000000"
};

const DEPLOYER_ADDRESS = "0x0000000000000000000000000000000000000010";
const GOVERNANCE_ADDRESS = "0x0000000000000000000000000000000000000020";
const PARLIA_ADDRESS = "0x0000000000000000000000000000000000000030";

const SYSTEM_CONTRACTS = [
  {
    address: DEPLOYER_ADDRESS,
    artifactPath: "./build/contracts/DeployerV1.json",
  },
  {
    address: GOVERNANCE_ADDRESS,
    artifactPath: "./build/contracts/GovernanceV1.json",
  },
  {
    address: PARLIA_ADDRESS,
    artifactPath: "./build/contracts/ParliaV1.json",
  },
];

const createExtraData = (validators) => {
  const zeroVanity = '0'.repeat(64);
  const validatorList = validators.map(v => {
    return v.startsWith('0x') ? v.substr(2) : v
  }).join('')
  const zeroSeal = '0'.repeat(130);
  return `0x${zeroVanity}${validatorList}${zeroSeal}`
};

const createGenesisConfig = (chainId, validators) => {
  const genesisConfig = Object.assign({}, GENESIS_CONFIG);
  for (const {address, artifactPath} of SYSTEM_CONTRACTS) {
    const {deployedBytecode} = require(artifactPath);
    const balance = {
      balance: '0x00',
      code: deployedBytecode,
    };
    // if (address === PARLIA_ADDRESS) {
    //   const PARLIA_VALIDATOR_MAP_OFFSET = 50,
    //     PARLIA_VALIDATOR_ARRAY_OFFSET = 51;
    //   const storage = {};
    //   validators.forEach((v, i) => {
    //     // fill mapping
    //     const mappingOffset = eth.toBuffer(utils.padLeft(v, 64)),
    //       mappingKey = eth.toBuffer(utils.padLeft(PARLIA_VALIDATOR_MAP_OFFSET, 64))
    //     const mappingHash = eth.keccak256(Buffer.concat([mappingOffset, mappingKey])).toString('hex')
    //     storage[`0x${mappingHash}`] = `0x000000000000000000000001${v.substr(2)}`;
    //     // fill array
    //     const arrayKeyOffset = eth.keccak256(eth.toBuffer(utils.padLeft(PARLIA_VALIDATOR_ARRAY_OFFSET, 64))),
    //       arrayIndexKey = utils.padLeft(new BigNumber(arrayKeyOffset.toString('hex'), 16).plus(i).toString(16), 64)
    //     storage[`0x${arrayIndexKey}`] = utils.padLeft(v, 64);
    //   });
    //   const arraySizeHash = eth.toBuffer(utils.padLeft(PARLIA_VALIDATOR_ARRAY_OFFSET, 64))
    //   storage[`0x${arraySizeHash.toString('hex')}`] = utils.padLeft(validators.length, 64);
    //   balance.storage = storage;
    // }
    genesisConfig.alloc[address] = balance;
  }
  genesisConfig['config']['chainId'] = chainId;
  genesisConfig['extraData'] = createExtraData(validators);
  return genesisConfig;
};

(async () => {
  const genesisMainnet = createGenesisConfig(17242, []);
  const genesisTestnet = createGenesisConfig(17243, [
    '0x00A601f45688DbA8a070722073B015277cF36725',
  ]);
  fs.writeFileSync('./mainnet/genesis.json', JSON.stringify(genesisMainnet, null, 2));
  fs.writeFileSync('./testnet/genesis.json', JSON.stringify(genesisTestnet, null, 2));
})();
