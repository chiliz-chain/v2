/** @var web3 {Web3} */

const createAndExecuteInstantProposal = async (
  // contracts
  governance,
  // proposal
  targets,
  values,
  calldatas,
  desc,
  // sender
  sender,
) => {
  console.log(`Creating proposal: ${desc}`)
  const currentOwner = await governance.getOwner()
  if (currentOwner === '0x0000000000000000000000000000000000000000') await governance.obtainOwnership();
  const votingPower = await governance.getVotingPower(sender)
  if (votingPower.toString() === '0') await governance.setVotingPower(sender, '1000', {from: sender})
  const {logs: [{args: {proposalId}}]} = await governance.propose(targets, values, calldatas, desc, {from: sender})
  await governance.castVote(proposalId, 1, {from: sender})
  return await governance.execute(targets, values, calldatas, web3.utils.keccak256(desc), {from: sender},);
}

const randomProposalDesc = () => {
  return `${(Math.random() * 10000) | 0}`
}

const addDeployer = async (governance, deployer, user, sender) => {
  const abi = deployer.contract.methods.addDeployer(user).encodeABI()
  return createAndExecuteInstantProposal(governance, [deployer.address], ['0x00'], [abi], `Add ${user} deployer (${randomProposalDesc()})`, sender)
}

const removeDeployer = async (governance, deployer, user, sender) => {
  const abi = deployer.contract.methods.removeDeployer(user).encodeABI()
  return createAndExecuteInstantProposal(governance, [deployer.address], ['0x00'], [abi], `Add ${user} deployer (${randomProposalDesc()})`, sender)
}

const registerDeployedContract = async (governance, deployer, owner, contract, sender) => {
  const abi = deployer.contract.methods.registerDeployedContract(owner, contract).encodeABI();
  return createAndExecuteInstantProposal(governance, [deployer.address], ['0x00'], [abi], `Register ${contract} deployed contract (${randomProposalDesc()})`, sender)
}

module.exports = {
  addDeployer,
  removeDeployer,
  registerDeployedContract,
  createAndExecuteInstantProposal
}