/** @var artifacts {Array} */
/** @var web3 {Web3} */
/** @function contract */
/** @function it */
/** @function before */
/** @var assert */

const Deployer = artifacts.require("DeployerV1");
const Governance = artifacts.require("GovernanceV1");
const Parlia = artifacts.require("ParliaV1");

contract("Injector", async (accounts) => {
  it("migration is working fine", async () => {
    const deployer = await Deployer.deployed();
    const governance = await Governance.deployed();
    const parlia = await Parlia.deployed();
    assert.equal(deployer.address, await deployer.getDeployer());
    assert.equal(deployer.address, await governance.getDeployer());
    assert.equal(deployer.address, await parlia.getDeployer());
    assert.equal(governance.address, await deployer.getGovernance());
    assert.equal(governance.address, await governance.getGovernance());
    assert.equal(governance.address, await parlia.getGovernance());
    assert.equal(parlia.address, await deployer.getParlia());
    assert.equal(parlia.address, await governance.getParlia());
    assert.equal(parlia.address, await parlia.getParlia());
  });
});
