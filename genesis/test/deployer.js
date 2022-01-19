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
  const [owner] = accounts;
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
  it("contract deployment is not possible w/o whitelist", async () => {
    const deployer = await Deployer.deployed()
    try {
      await deployer.registerDeployedContract(owner, '0x0000000000000000000000000000000000000123', {
        from: owner,
      });
      assert.fail()
    } catch (e) {
      assert.equal(e.message.includes('Deployer: deployer is not allowed'), true)
    }
    await deployer.addDeployer(owner)
    const r1 = await deployer.registerDeployedContract(owner, '0x0000000000000000000000000000000000000123', {
      from: owner,
    });
    assert.equal(r1.logs.length, 1)
    assert.equal(r1.logs[0].event, 'ContractDeployed')
    assert.deepEqual(r1.logs[0].args.account, owner)
  })
});
