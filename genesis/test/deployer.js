/** @var artifacts {Array} */
/** @var web3 {Web3} */
/** @function contract */
/** @function it */
/** @function before */
/** @var assert */

const Deployer = artifacts.require("Deployer");
const Governance = artifacts.require("Governance");
const Parlia = artifacts.require("Parlia");

const {addDeployer, removeDeployer, registerDeployedContract} = require('./helper')

contract("Deployer", async (accounts) => {
  const [owner] = accounts;
  it("add remove deployer", async () => {
    const governance = await Governance.deployed(),
      deployer = await Deployer.deployed();
    assert.equal(await deployer.isDeployer('0x0000000000000000000000000000000000000001'), false)
    // add deployer
    const r1 = await addDeployer(governance, deployer, '0x0000000000000000000000000000000000000001', owner)
    const [, log1] = r1.receipt.rawLogs
    assert.equal(log1.data, '0x0000000000000000000000000000000000000000000000000000000000000001')
    assert.equal(await deployer.isDeployer('0x0000000000000000000000000000000000000001'), true)
    // remove deployer
    const r2 = await removeDeployer(governance, deployer, '0x0000000000000000000000000000000000000001', owner)
    const [, log2] = r2.receipt.rawLogs
    assert.equal(log2.data, '0x0000000000000000000000000000000000000000000000000000000000000001')
    assert.equal(await deployer.isDeployer('0x0000000000000000000000000000000000000001'), false)
  });
  it("contract deployment is not possible w/o whitelist", async () => {
    const governance = await Governance.deployed(),
      deployer = await Deployer.deployed();
    try {
      await registerDeployedContract(governance, deployer, owner, '0x0000000000000000000000000000000000000123', owner);
      assert.fail()
    } catch (e) {
      assert.equal(e.message.includes('Deployer: deployer is not allowed'), true)
    }
    // let owner be a deployer
    await addDeployer(governance, deployer, owner, owner)
    const r1 = await registerDeployedContract(governance, deployer, owner, '0x0000000000000000000000000000000000000123', owner);
    const [, log1] = r1.receipt.rawLogs
    assert.equal(log1.data.toLowerCase(), `0x000000000000000000000000${owner.substr(2)}0000000000000000000000000000000000000000000000000000000000000123`.toLowerCase())
    assert.equal(log1.topics[0], web3.utils.keccak256('ContractDeployed(address,address)'))
  })
});
