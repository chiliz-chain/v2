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
    mapping(address => uint256) private _collectedFees;

    function init() public override {
        super.init();
        // TODO: "remove this validator in the future"
        addValidator(0x00A601f45688DbA8a070722073B015277cF36725);
    }

    function isValidator(address account) public override view returns (bool) {
        return _validatorsMap[account].exists;
    }

    function addValidator(address account) public onlyGovernance override {
        require(!_validatorsMap[account].exists, "Parlia: validator already exist");
        _validatorsMap[account] = Validator({
        exists : true,
        account : account
        });
        _validators.push(account);
        emit ValidatorAdded(account);
    }

    function removeValidator(address account) public onlyGovernance override {
        require(_validatorsMap[account].exists, "Parlia: validator doesn't exist");
        delete _validatorsMap[account];
        emit ValidatorRemoved(account);
    }

    function getValidators() external view override returns (address[] memory) {
        return _validators;
    }

    function deposit(address validator) public payable onlyCoinbase override {
        require(msg.value > 0, "Parlia: deposit is zero");
        _collectedFees[validator] += msg.value;
    }

    function claimDepositFee(address payable validator) public override {
        uint256 totalFee = _collectedFees[validator];
        require(totalFee > 0, "Parlia: deposited fee is zero");
        _collectedFees[validator] = 0;
        require(validator.send(totalFee), "Parlia: transfer failed");
    }

    function slash(address /*validator*/) external view onlyCoinbase override {
        revert("not implemented");
    }
}
