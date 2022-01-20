// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "./Injector.sol";

contract Parlia is IParlia, InjectorContextHolderV1 {

    event ValidatorAdded(address account);
    event ValidatorRemoved(address account);

    struct Validator {
        bool exists;
        address account;
    }

    mapping(address => Validator) private _validatorsMap;
    address[] private _validators;
    mapping(address => uint256) private _collectedFees;

    constructor(address[] memory validators) {
        for (uint256 i = 0; i < validators.length; i++) {
            _addValidator(validators[i]);
        }
    }

    function isValidator(address account) public override view returns (bool) {
        return _validatorsMap[account].exists;
    }

    function addValidator(address account) public onlyFromGovernance override {
        _addValidator(account);
    }

    function _addValidator(address account) internal {
        require(!_validatorsMap[account].exists, "Parlia: validator already exist");
        _validatorsMap[account] = Validator({
        exists : true,
        account : account
        });
        _validators.push(account);
        emit ValidatorAdded(account);
    }

    function removeValidator(address account) public onlyFromGovernance override {
        require(_validatorsMap[account].exists, "Parlia: validator doesn't exist");
        delete _validatorsMap[account];
        emit ValidatorRemoved(account);
    }

    function getValidators() external view override returns (address[] memory) {
        return _validators;
    }

    function deposit(address validator) public payable onlyFromCoinbaseOrGovernance override {
        require(msg.value > 0, "Parlia: deposit is zero");
        _collectedFees[validator] += msg.value;
    }

    function claimDepositFee(address payable validator) public override {
        uint256 totalFee = _collectedFees[validator];
        require(totalFee > 0, "Parlia: deposited fee is zero");
        _collectedFees[validator] = 0;
        require(validator.send(totalFee), "Parlia: transfer failed");
    }

    function slash(address /*validator*/) external view onlyFromCoinbaseOrGovernance override {
        revert("not implemented");
    }

    receive() external payable {
    }
}
