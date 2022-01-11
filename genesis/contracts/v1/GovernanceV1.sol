// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "../Injector.sol";

contract GovernanceV1 is IGovernance, Ownable, InjectorContextHolderV1 {

    string public constant name = "Governance";

    function votingDelay() public pure returns (uint) {
        return 0;
    }

    // ~3 days in blocks (assuming 15s blocks)

    function votingPeriod() public pure returns (uint) {
        return 20000;
    }

    uint public proposalCount;

    struct Proposal {
        uint id;
        address proposer;
        address target;
        bytes callArg;
        uint startBlock;
        uint endBlock;
        uint forVotes;
        uint totalVoters;
        bool canceled;
        bool executed;
    }

    struct Receipt {
        bool hasVoted;
        bool support;
        uint96 votes;
    }

    enum ProposalState {
        Pending,
        Active,
        Canceled,
        Succeeded,
        Executed,
        Expired
    }

    mapping(uint => mapping(address => Receipt)) receipts;
    mapping(uint => Proposal) public proposals;
    mapping(address => uint) public latestProposalIds;

    event ProposalCreated(uint id, address proposer, address target, bytes callArg, uint startBlock, uint endBlock, string description);
    event VoteCast(address voter, uint proposalId, bool support, uint votes);
    event ProposalExecuted(uint id);
    event ProposalCanceled(uint id);

    function propose(address target, bytes memory callArg, string memory description) onlyValidator public returns (uint) {
        uint latestProposalId = latestProposalIds[msg.sender];
        if (latestProposalId != 0) {
            ProposalState proposersLatestProposalState = state(latestProposalId);
            require(proposersLatestProposalState != ProposalState.Active, "Governance::propose: one live proposal per proposer, found an already active proposal");
            require(proposersLatestProposalState != ProposalState.Pending, "Governance::propose: one live proposal per proposer, found an already pending proposal");
        }

        uint startBlock = block.number + votingDelay();
        uint endBlock = startBlock + votingPeriod();
        uint totalVoters = getTotalVoters();

        proposalCount++;
        Proposal memory newProposal = Proposal({
        id : proposalCount,
        proposer : msg.sender,
        target : target,
        callArg : callArg,
        startBlock : startBlock,
        endBlock : endBlock,
        totalVoters : totalVoters,
        forVotes : 0,
        canceled : false,
        executed : false
        });

        proposals[newProposal.id] = newProposal;
        latestProposalIds[newProposal.proposer] = newProposal.id;

        emit ProposalCreated(newProposal.id, msg.sender, target, callArg, startBlock, endBlock, description);
        return newProposal.id;
    }

    function getTotalVoters() public view returns (uint) {
        return getParlia().getValidators().length;
    }

    function execute(uint proposalId) onlyValidator public payable {
        require(state(proposalId) == ProposalState.Succeeded, "Governance: proposal can only be executed if it is succeeded");
        Proposal storage proposal = proposals[proposalId];
        proposal.executed = true;

        (bool success,) = proposal.target.call(proposal.callArg);
        require(success, "Governance: target execution reverted.");
        emit ProposalExecuted(proposalId);
    }

    function cancel(uint proposalId) public {
        Proposal storage proposal = proposals[proposalId];
        require(msg.sender == proposal.proposer, "Governance: only proposer can cancel proposal");
        require(proposal.forVotes == 0, "Governance: only proposals without votes can be cancelled");
        ProposalState proposalState = state(proposalId);
        require(proposalState != ProposalState.Executed, "Governance: cannot cancel executed proposal");
        proposal.canceled = true;
        emit ProposalCanceled(proposalId);
    }

    function getActions(uint proposalId) public view returns (address target, bytes memory callArg) {
        Proposal storage p = proposals[proposalId];
        return (p.target, p.callArg);
    }

    function getReceipt(uint proposalId, address voter) public view returns (Receipt memory) {
        return receipts[proposalId][voter];
    }

    function requiredVotes(uint proposalId) public view returns (uint) {
        Proposal storage p = proposals[proposalId];
        return (((p.totalVoters * 2) + 2) / 3);
    }

    function state(uint proposalId) public view returns (ProposalState) {
        require(proposalCount >= proposalId && proposalId > 0, "Governance::state: invalid proposal id");
        Proposal storage proposal = proposals[proposalId];
        if (proposal.canceled) {
            return ProposalState.Canceled;
        } else if (block.number <= proposal.startBlock) {
            return ProposalState.Pending;
        } else if (proposal.executed) {
            return ProposalState.Executed;
        } else if (proposal.forVotes >= requiredVotes(proposalId)) {
            return ProposalState.Succeeded;
        } else if (block.number > proposal.endBlock) {
            return ProposalState.Expired;
        } else {
            return ProposalState.Active;
        }
    }

    function vote(uint proposalId) onlyValidator public {
        address voter = msg.sender;
        require(state(proposalId) == ProposalState.Active, "Governance: voting is not active");
        Proposal storage proposal = proposals[proposalId];
        Receipt storage receipt = receipts[proposalId][voter];
        require(receipt.hasVoted == false, "Governance: voter already voted");
        uint96 votes = 1;
        proposal.forVotes = proposal.forVotes + votes;
        receipt.hasVoted = true;
        receipt.support = true;
        receipt.votes = votes;
        emit VoteCast(voter, proposalId, true, votes);
    }
}


