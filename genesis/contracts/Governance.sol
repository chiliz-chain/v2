// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "@openzeppelin/contracts/governance/extensions/GovernorSettings.sol";
import "@openzeppelin/contracts/governance/extensions/GovernorCountingSimple.sol";
import "@openzeppelin/contracts/governance/extensions/GovernorVotes.sol";

import "./Injector.sol";

contract Governance is InjectorContextHolderV1, GovernorCountingSimple, GovernorSettings, IGovernance {

    event VotingPowerSet(address voter, uint256 power, uint256 supply);

    address private _owner;
    mapping(address => uint256) private _votingPower;
    uint256 private _votingSupply;

    constructor(address owner, uint256 votingPeriod) Governor("Chiliz Governance") GovernorSettings(0, votingPeriod, 0) {
        _owner = owner;
    }

    function getOwner() external view returns (address) {
        return _owner;
    }

    function getVotingPower(address voter) external view returns (uint256) {
        return _votingPower[voter];
    }

    function getVotingSupply() external view returns (uint256) {
        return _votingSupply;
    }

    modifier onlyOwner() {
        require(msg.sender == _owner, "Governance: only owner");
        _;
    }

    function setVotingPower(address voter, uint256 votingPower) external onlyOwner {
        uint256 votingPowerBefore = _votingPower[voter];
        _votingPower[voter] = votingPower;
        _votingSupply = _votingSupply + votingPower - votingPowerBefore;
        emit VotingPowerSet(voter, votingPower, _votingSupply);
    }

    function getVotes(address account, uint256 /*blockNumber*/) public view override returns (uint256) {
        return _votingPower[account];
    }

    function quorum(uint256 /*blockNumber*/) public view override returns (uint256) {
        return _votingSupply * 2 / 3;
    }

    function votingDelay() public view override(IGovernor, GovernorSettings) returns (uint256) {
        return GovernorSettings.votingDelay();
    }

    function proposalThreshold() public view virtual override(Governor, GovernorSettings) returns (uint256) {
        return GovernorSettings.proposalThreshold();
    }

    function votingPeriod() public view override(IGovernor, GovernorSettings) returns (uint256) {
        return GovernorSettings.votingPeriod();
    }
}