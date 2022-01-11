require("dotenv").config();
const {spawnSync} = require("child_process");

const getNetworkImpl = async () => {
    const networkId = await web3.eth.net.getId()
    let implFile, networkName
    switch (networkId) {
        case 5: {
            implFile = require('./../.openzeppelin/goerli.json')
            networkName = 'goerli'
            break;
        }
        case 56: {
            implFile = require('./../.openzeppelin/unknown-56.json')
            networkName = 'smartchain'
            break;
        }
        case 97: {
            implFile = require('./../.openzeppelin/unknown-97.json')
            networkName = 'smartchaintestnet'
            break;
        }
        default: {
            throw new Error(`Unknown network ${networkId}`)
        }
    }
    return [implFile, networkName];
};

module.exports = async function (done) {
    try {
        const [implFile, networkName] = await getNetworkImpl();
        const impls = Object.keys(implFile.impls).reverse()
        for (const impl of impls) {
            const implObj = implFile.impls[impl]
            const storage = implObj.layout.storage
            const contract = storage[storage.length - 1].contract
            const address = implObj.address
            try {
                const a = await spawnSync('npx', ['truffle', 'run', 'verify', `${contract}@${address}`, '--network', networkName])
                console.log(a.stdout.toString())
            } catch (e) {
                console.log(`Couldn't verify ${contract}`)
                done(e)
            }
        }
    } catch (e) {
        console.error(`Failed to verify contract: ${e}`);
        done(e)
    }
    done();
};