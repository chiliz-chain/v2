// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "../Injector.sol";

contract GovernanceV1 is IGovernance, InjectorContextHolderV1 {

    address private _owner;

    constructor() {
        _owner = msg.sender;
    }

    function obtainOwnership() external {
        require(_owner == address(0x00), "Governance: already obtained");
        _owner = msg.sender;
    }

    function transferOwnership(address owner) external onlyOwner {
        _owner = owner;
    }

    modifier onlyOwner() {
        require(_owner == msg.sender, "Governance: only owner");
        _;
    }
}


