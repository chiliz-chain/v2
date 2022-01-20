/** @var artifacts {Array} */
/** @var web3 {Web3} */
/** @function contract */
/** @function it */
/** @function before */
/** @var assert */

const Deployer = artifacts.require("Deployer");
const Governance = artifacts.require("Governance");
const Parlia = artifacts.require("Parlia");

contract("Governance", async (accounts) => {
  const [owner, voter, deployer] = accounts
  it("ownership can be only obtained once", async () => {
    const governance = await Governance.deployed();
    assert.equal(await governance.getOwner(), '0x0000000000000000000000000000000000000000')
    await governance.obtainOwnership();
    assert.equal(await governance.getOwner(), owner)
    try {
      await governance.obtainOwnership();
      assert.fail()
    } catch (e) {
      assert.equal(e.message.includes('Governance: owner is already assigned'), true)
    }
  })
  it("voting power is managed by owner", async () => {
    const governance = await Governance.deployed();
    const r1 = await governance.setVotingPower(owner, '1000')
    assert.equal(r1.logs[0].event, 'VotingPowerSet')
    assert.equal(r1.logs[0].args.voter, owner)
    assert.equal(r1.logs[0].args.power, '1000')
    assert.equal(r1.logs[0].args.supply, '1000')
    assert.equal(await governance.getVotingSupply(), '1000')
    const r2 = await governance.setVotingPower(voter, '1000')
    assert.equal(r2.logs[0].event, 'VotingPowerSet')
    assert.equal(r2.logs[0].args.voter, voter)
    assert.equal(r2.logs[0].args.power, '1000')
    assert.equal(r2.logs[0].args.supply, '2000')
    assert.equal(await governance.getVotingSupply(), '2000')
    const r3 = await governance.setVotingPower(voter, '500')
    assert.equal(r3.logs[0].event, 'VotingPowerSet')
    assert.equal(r3.logs[0].args.voter, voter)
    assert.equal(r3.logs[0].args.power, '500')
    assert.equal(r3.logs[0].args.supply, '1500')
    assert.equal(await governance.getVotingSupply(), '1500')
    const quorum = await governance.quorum(0)
    assert.equal(quorum.toString(), '1000')
  });
  it("simple proposal should work", async () => {
    const governance = await Governance.deployed(),
      deployer = await Deployer.deployed();
    const r1 = await governance.propose(
      [deployer.address],
      ['0x00'],
      [deployer.contract.methods.addDeployer(owner).encodeABI()],
      'Whitelist new deployer')
    const {proposalId} = r1.logs[0].args
    assert.equal(r1.logs[0].event, 'ProposalCreated')
    const descriptionHash = web3.utils.keccak256('Whitelist new deployer')
    const r2 = await governance.castVote(proposalId, 1)
    assert.equal(r2.logs[0].event, 'VoteCast')
    const r3 = await governance.execute(
      [deployer.address],
      ['0x00'],
      [deployer.contract.methods.addDeployer(owner).encodeABI()],
      descriptionHash,
    );
    assert.equal(r3.logs[0].event, 'ProposalExecuted')
  })
});
