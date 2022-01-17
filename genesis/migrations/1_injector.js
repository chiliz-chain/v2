const Deployer = artifacts.require("DeployerV1");
const Governance = artifacts.require("GovernanceV1");
const Parlia = artifacts.require("ParliaV1");

module.exports = async function (deployer) {
  // deploy dependencies
  await deployer.deploy(Deployer);
  const deployerV1 = await Deployer.deployed();
  await deployer.deploy(Governance);
  const governanceV1 = await Governance.deployed();
  await deployer.deploy(Parlia);
  const parliaV1 = await Parlia.deployed();
  // init injector with deps
  await deployerV1.initManually(deployerV1.address, governanceV1.address, parliaV1.address);
  await governanceV1.initManually(deployerV1.address, governanceV1.address, parliaV1.address);
  await parliaV1.initManually(deployerV1.address, governanceV1.address, parliaV1.address);
};
