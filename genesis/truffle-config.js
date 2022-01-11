const HDWalletProvider = require('@truffle/hdwallet-provider');

module.exports = {

  networks: {
    ganache: {
      provider: () => new HDWalletProvider({
        privateKeys: [
          '<<-- PUT YOUR PRIVATE KEY -->>',
        ],
        providerOrUrl: "http://127.0.0.1:8545/"
      }),
      network_id: 5,
      confirmations: 1,
      gas: 8000000,
      timeoutBlocks: 50,
      skipDryRun: true,
      networkCheckTimeout: 10000000
    },
    smartchaintestnet: {
      provider: () => new HDWalletProvider({
        privateKeys: [
          '<<-- PUT YOUR PRIVATE KEY -->>',
        ],
        providerOrUrl: "https://data-seed-prebsc-1-s2.binance.org:8545/"
      }),
      network_id: 97,
      confirmations: 1,
      gas: 8000000,
      timeoutBlocks: 50,
      skipDryRun: true,
      networkCheckTimeout: 10000000
    },
    goerli: {
      provider: () => new HDWalletProvider({
        privateKeys: [
          '<<-- PUT YOUR PRIVATE KEY -->>',
        ],
        providerOrUrl: "https://eth-goerli-01.dccn.ankr.com"
      }),
      network_id: 5,
      confirmations: 1,
      gas: 8000000,
      timeoutBlocks: 50,
      skipDryRun: true,
      networkCheckTimeout: 10000000
    },
    smartchain: {
      provider: () => new HDWalletProvider({
        privateKeys: [
          '<<-- PUT YOUR PRIVATE KEY -->>',
        ],
        providerOrUrl: "https://bsc-dataseed.binance.org/"
      }),
      network_id: 56,
      gas: 8000000,
      confirmations: 1,
      gasPrice: 20000000000,
      timeoutBlocks: 50,
      skipDryRun: true,
      networkCheckTimeout: 10000000
    },
  },

  // Set default mocha options here, use special reporters etc.
  mocha: {
    // timeout: 100000
  },

  // Configure your compilers
  compilers: {
    solc: {
      version: "0.8.3",    // Fetch exact version from solc-bin (default: truffle's version)
      settings: {          // See the solidity docs for advice about optimization and evmVersion
       optimizer: {
         enabled: true,
         runs: 200
       },
      }
    }
  }
};
