/** @var artifacts {Array} */
/** @var web3 {Web3} */
/** @function contract */
/** @function it */
/** @function before */
/** @var assert */

const Deployer = artifacts.require("DeployerV1");
const Governance = artifacts.require("GovernanceV1");
const Parlia = artifacts.require("ParliaV1");

contract("Deployer", async (accounts) => {
  it("add remove deployer", async () => {
    const deployer = await Deployer.deployed();
    assert.equal(await deployer.isDeployer('0x0000000000000000000000000000000000000001'), false)
    const {logs} = await deployer.addDeployer('0x0000000000000000000000000000000000000001')
    assert.equal(logs.length, 1)
    assert.deepEqual(logs[0].event, 'DeployerAdded')
    assert.deepEqual(logs[0].args.account, '0x0000000000000000000000000000000000000001')
    assert.equal(await deployer.isDeployer('0x0000000000000000000000000000000000000001'), true)
    const {logs: logs2} = await deployer.removeDeployer('0x0000000000000000000000000000000000000001')
    assert.equal(logs2.length, 1)
    assert.deepEqual(logs2[0].event, 'DeployerRemoved')
    assert.deepEqual(logs2[0].args.account, '0x0000000000000000000000000000000000000001')
    assert.equal(await deployer.isDeployer('0x0000000000000000000000000000000000000001'), false)
  });

});
