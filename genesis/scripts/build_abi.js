const fs = require("fs");
const path = require("path");

const buildPath = path.join(__dirname, "../build/contracts");
const addresses = {};

fs.readdirSync(buildPath).forEach(val => {
    let {contractName, abi, networks} = require(path.join(buildPath, val));
    /* lets don't generate abi for builds w/o network since its not deployed */
    for (const [network, {address}] of Object.entries(networks)) {
        if (!addresses.hasOwnProperty(network)) {
            addresses[network] = {};
        }
        addresses[network][contractName] = address;
    }
    const revisionIndex = contractName.lastIndexOf("_R");
    if (revisionIndex > -1) {
        contractName = contractName.slice(0, revisionIndex);
    }
    fs.writeFileSync(path.join(__dirname, `../build/abi/${contractName}.json`), JSON.stringify(abi, null, 2));
});

fs.writeFileSync(path.join(__dirname, `../build/addresses.json`), JSON.stringify(addresses, null, 2));
