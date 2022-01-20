const Deployer = artifacts.require("Deployer");
const Governance = artifacts.require("Governance");
const Parlia = artifacts.require("Parlia");

module.exports = async function (deployer) {
  const [deployerAccount] = await web3.eth.getAccounts()
  // deploy dependencies
  await deployer.deploy(Deployer, []);
  const deployerContract = await Deployer.deployed();
  await deployer.deploy(Governance, deployerAccount);
  const governanceContract = await Governance.deployed();
  await deployer.deploy(Parlia, []);
  const parliaContract = await Parlia.deployed();
  // init injector with deps
  await deployerContract.initManually(deployerContract.address, governanceContract.address, parliaContract.address);
  await governanceContract.initManually(deployerContract.address, governanceContract.address, parliaContract.address);
  await parliaContract.initManually(deployerContract.address, governanceContract.address, parliaContract.address);
  // print addresses
  console.log(`Deployer: ${deployerContract.address}`)
  console.log(`Governance: ${governanceContract.address}`)
  console.log(`Parlia: ${parliaContract.address}`)
};
