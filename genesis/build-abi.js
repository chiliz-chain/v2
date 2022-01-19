const fs = require("fs");
const path = require("path");

const buildPath = path.join(__dirname, "./build/contracts");
const addresses = {};

fs.readdirSync(buildPath).forEach(val => {
    let {contractName, abi, networks} = require(path.join(buildPath, val));
    for (const [network, {address}] of Object.entries(networks)) {
        if (!addresses.hasOwnProperty(network)) {
            addresses[network] = {};
        }
        addresses[network][contractName] = address;
    }
    fs.mkdirSync(path.join(__dirname, `./build/abi/`), {
        recursive: true,
    })
    fs.writeFileSync(path.join(__dirname, `./build/abi/${contractName}.json`), JSON.stringify(abi, null, 2));
});

fs.writeFileSync(path.join(__dirname, `./build/addresses.json`), JSON.stringify(addresses, null, 2));
