// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "./Injector.sol";

contract Deployer is IDeployer, InjectorContextHolderV1 {

    event DeployerAdded(address account);
    event DeployerRemoved(address account);
    event DeployerBanned(address account);
    event DeployerUnbanned(address account);

    event ContractDeployed(address account, address impl);

    struct ContractDeployer {
        bool exists;
        address account;
        bool banned;
    }

    enum ContractState {
        Disabled,
        Enabled
    }

    struct SmartContract {
        ContractState state;
        address impl;
        address deployer;
    }

    constructor(address[] memory deployers) {
        for (uint256 i = 0; i < deployers.length; i++) {
            _addDeployer(deployers[i]);
        }
    }

    mapping(address => ContractDeployer) private _contractDeployers;
    mapping(address => SmartContract) private _smartContracts;

    function isDeployer(address account) public override view returns (bool) {
        return _contractDeployers[account].exists;
    }

    function isBanned(address account) public override view returns (bool) {
        return _contractDeployers[account].banned;
    }

    function addDeployer(address account) public onlyFromGovernance override {
        _addDeployer(account);
    }

    function _addDeployer(address account) internal {
        require(!_contractDeployers[account].exists, "Deployer: deployer already exist");
        _contractDeployers[account] = ContractDeployer({
        exists : true,
        account : account,
        banned : false
        });
        emit DeployerAdded(account);
    }

    function removeDeployer(address account) public onlyFromGovernance override {
        require(_contractDeployers[account].exists, "Deployer: deployer doesn't exist");
        delete _contractDeployers[account];
        emit DeployerRemoved(account);
    }

    function banDeployer(address account) public onlyFromGovernance override {
        require(_contractDeployers[account].exists, "Deployer: deployer doesn't exist");
        require(!_contractDeployers[account].banned, "Deployer: deployer already banned");
        _contractDeployers[account].banned = true;
        emit DeployerBanned(account);
    }

    function unbanDeployer(address account) public onlyFromGovernance override {
        require(_contractDeployers[account].exists, "Deployer: deployer doesn't exist");
        require(_contractDeployers[account].banned, "Deployer: deployer is not banned");
        _contractDeployers[account].banned = false;
        emit DeployerUnbanned(account);
    }

    function getContractDeployer(address contractAddress) public view override returns (uint8 state, address impl, address deployer) {
        SmartContract memory dc = _smartContracts[contractAddress];
        state = uint8(dc.state);
        impl = dc.impl;
        deployer = dc.deployer;
    }

    function registerDeployedContract(address deployer, address impl) public onlyFromCoinbaseOrGovernance override {
        // make sure this call is allowed
        require(isDeployer(deployer), "Deployer: deployer is not allowed");
        // remember who deployed contract
        SmartContract memory dc = _smartContracts[impl];
        require(dc.impl == address(0x00), "Deployer: contract is deployed already");
        dc.state = ContractState.Enabled;
        dc.impl = impl;
        dc.deployer = deployer;
        _smartContracts[impl] = dc;
        // emit event
        emit ContractDeployed(deployer, impl);
    }

    function checkContractActive(address impl) external view onlyFromCoinbaseOrGovernance override {
        // for non-contract just exist
        if (!Address.isContract(impl)) {
            return;
        }
        // check that contract is enabled
        SmartContract memory dc = _smartContracts[impl];
        require(dc.state == ContractState.Enabled, "Deployer: contract is not enabled");
        // check is deployer still active (don't allow to make calls to contracts deployed by disabled deployers)
        require(!isBanned(dc.deployer), "Deployer: contract is disabled");
    }
}
