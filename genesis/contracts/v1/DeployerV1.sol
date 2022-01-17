// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "../Injector.sol";

contract DeployerV1 is IDeployer, InjectorContextHolderV1 {

    event DeployerAdded(address account);
    event DeployerRemoved(address account);
    event DeployerBanned(address account);
    event DeployerUnbanned(address account);

    event ContractDeployed(address account, address impl);

    struct Deployer {
        bool exists;
        address account;
        bool banned;
    }

    enum State {
        Disabled,
        Enabled
    }

    mapping(address => address[]) private _deployedContracts;
    mapping(address => address) private _contractDeployer;
    mapping(address => Deployer) private _deployers;
    mapping(address => State) private _contractState;

    function isDeployer(address account) public override view returns (bool) {
        return _deployers[account].exists;
    }

    function isBanned(address account) public override view returns (bool) {
        return _deployers[account].banned;
    }

    function addDeployer(address account) public onlyGovernance override {
        require(!_deployers[account].exists, "Governance: deployer already exist");
        _deployers[account] = Deployer({
        exists : true,
        account : account,
        banned : false
        });
        emit DeployerAdded(account);
    }

    function removeDeployer(address account) public onlyGovernance override {
        require(_deployers[account].exists, "Governance: deployer doesn't exist");
        delete _deployers[account];
        emit DeployerRemoved(account);
    }

    function banDeployer(address account) public onlyGovernance override {
        require(_deployers[account].exists, "Governance: deployer doesn't exist");
        require(!_deployers[account].banned, "Governance: deployer already banned");
        _deployers[account].banned = true;
        emit DeployerBanned(account);
    }

    function unbanDeployer(address account) public onlyGovernance override {
        require(_deployers[account].exists, "Governance: deployer doesn't exist");
        require(_deployers[account].banned, "Governance: deployer is not banned");
        _deployers[account].banned = false;
        emit DeployerUnbanned(account);
    }

    function getContractDeployer(address impl) public view override returns (address) {
        return _contractDeployer[impl];
    }

    function registerDeployedContract(address account, address impl) public onlyCoinbase override {
        // make sure this call is allowed
        require(isDeployer(account), "Deployer: deployer is not allowed");
        // remember who deployed contract
        require(_contractDeployer[impl] == address(0x00), "Deployer: contract is deployed already");
        _contractDeployer[impl] = account;
        // lets keep list of all deployed contracts
        _deployedContracts[account].push(impl);
        // enable this contract by default
        _contractState[impl] = State.Enabled;
        // emit event
        emit ContractDeployed(account, impl);
    }

    function checkContractActive(address impl) external view onlyCoinbase override {
        // for non-contract just exist
        if (!Address.isContract(impl)) {
            return;
        }
        // check that contract is enabled
        require(_contractState[impl] == State.Enabled, "Deployer: contract is not enabled");
        // make sure contract exists
        address deployer = _contractDeployer[impl];
        require(deployer != address(0x00), "Deployer: contract is not registered");
        // check is deployer still active (don't allow to make calls to contracts deployed by disabled deployers)
        require(!isBanned(deployer), "Deployer: contract is disabled");
    }
}
