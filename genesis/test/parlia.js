/** @var artifacts {Array} */
/** @var web3 {Web3} */
/** @function contract */
/** @function it */
/** @function before */
/** @var assert */

const Deployer = artifacts.require("Deployer");
const Governance = artifacts.require("Governance");
const Parlia = artifacts.require("Parlia");

contract("Parlia", async (accounts) => {
  it("add remove validator", async () => {
    const parlia = await Parlia.deployed();
    assert.equal(await parlia.isValidator('0x00A601f45688DbA8a070722073B015277cF36725'), false)
    const {logs} = await parlia.addValidator('0x00A601f45688DbA8a070722073B015277cF36725')
    assert.equal(logs.length, 1)
    assert.deepEqual(logs[0].event, 'ValidatorAdded')
    assert.deepEqual(logs[0].args.account, '0x00A601f45688DbA8a070722073B015277cF36725')
    assert.equal(await parlia.isValidator('0x00A601f45688DbA8a070722073B015277cF36725'), true)
  });
});
