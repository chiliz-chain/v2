// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "../Injector.sol";

contract ParliaV1 is IParlia, InjectorContextHolderV1 {

    event ValidatorAdded(address account);
    event ValidatorRemoved(address account);

    struct Validator {
        bool exists;
        address account;
    }

    mapping(address => Validator) private _validatorsMap;
    address[] private _validators;

    function isValidator(address account) public override view returns (bool) {
        return _validatorsMap[account].exists;
    }

    function addValidator(address account) public onlyGovernance override {
        _addValidator(account);
    }

    function _addValidator(address account) private {
        _validatorsMap[account] = Validator({
        exists : true,
        account : account
        });
        _validators.push(account);
        emit ValidatorAdded(account);
    }

    function removeValidator(address account) public onlyGovernance override {
        require(_validatorsMap[account].exists, "Governance: validator doesn't exist");
        delete _validatorsMap[account];
    }

    function getValidators() external view override returns (address[] memory) {
        return _validators;
    }

    function deposit(address /*validator*/) public payable onlyCoinbase override {
        require(msg.value > 0, "Parlia: deposit is zero");
        //
    }

    function initValidator(address account) public {
        require(_validators.length == 0, "can't init validators list if validators count is not 0");
        _addValidator(account);
    }
}
