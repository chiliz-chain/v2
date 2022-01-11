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
    const r1 = await deployer.addDeployer('0x0000000000000000000000000000000000000001')
    console.log(r1)
    assert.equal(await deployer.isDeployer('0x0000000000000000000000000000000000000001'), true)
    const r2 = await deployer.removeDeployer('0x0000000000000000000000000000000000000001')
    console.log(r2)
    assert.equal(await deployer.isDeployer('0x0000000000000000000000000000000000000001'), false)
  });
});
