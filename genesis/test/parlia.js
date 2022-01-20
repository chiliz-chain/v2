/** @var artifacts {Array} */
/** @var web3 {Web3} */
/** @function contract */
/** @function it */
/** @function before */
/** @var assert */

const Deployer = artifacts.require("Deployer");
const Governance = artifacts.require("Governance");
const Parlia = artifacts.require("Parlia");

const {addValidator, removeValidator} = require('./helper')

contract("Parlia", async (accounts) => {
  const [owner] = accounts
  it("add remove validator", async () => {
    const governance = await Governance.deployed(),
      parlia = await Parlia.deployed();
    assert.equal(await parlia.isValidator('0x00A601f45688DbA8a070722073B015277cF36725'), false)
    const {receipt: {rawLogs: rawLogs1}} = await addValidator(governance, parlia, '0x00A601f45688DbA8a070722073B015277cF36725', owner),
      [, log1] = rawLogs1
    assert.equal(log1.topics[0], web3.utils.keccak256('ValidatorAdded(address)'))
    assert.equal(log1.data, `0x00000000000000000000000000a601f45688dba8a070722073b015277cf36725`)
    assert.equal(await parlia.isValidator('0x00A601f45688DbA8a070722073B015277cF36725'), true)
    const {receipt: {rawLogs: rawLogs2}} = await removeValidator(governance, parlia, '0x00A601f45688DbA8a070722073B015277cF36725', owner),
      [, log2] = rawLogs2
    assert.equal(log2.topics[0], web3.utils.keccak256('ValidatorRemoved(address)'))
    assert.equal(log2.data, `0x00000000000000000000000000a601f45688dba8a070722073b015277cf36725`)
    assert.equal(await parlia.isValidator('0x00A601f45688DbA8a070722073B015277cF36725'), false)
  });
});
