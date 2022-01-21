import {action, makeAutoObservable} from "mobx";
import {Contract, PastEventOptions} from "web3-eth-contract";
import {IWeb3KeyProvider, IWeb3SendResult, Web3KeyProvider} from "@ankr.com/stakefi-web3";
import {keccak256} from "web3-utils"

export type Web3Uint256 = string
export type Web3Address = string

export interface IWeb3DefaultEvent {
  event: string;
  signature: string;
  logIndex: number;
  transactionIndex: number;
  transactionHash: string;
  blockHash: string;
  blockNumber: number;
  address: string;
}

export type Web3FetchEventLogsFn<T> = (options: PastEventOptions) => Promise<T[]>

export enum EProposalStatus {
  Pending,
  Active,
  Canceled,
  Defeated,
  Succeeded,
  Queued,
  Expired,
  Executed
}

export enum EProposalType {
  Unknown,
  AddDeployer,
  RemoveDeployer,
}

export type IProposalCreatedEvent = IWeb3DefaultEvent & {
  proposalId: Web3Uint256;
  proposer: Web3Address;
  targets: Web3Address[];
  values: Web3Uint256[];
  signatures: string[];
  calldatas: string[];
  startBlock: Web3Uint256;
  endBlock: Web3Uint256;
  description: string;
  status: EProposalStatus;
}

export type IProposalCanceledEvent = IWeb3DefaultEvent & {
  proposalId: Web3Uint256;
}

export type IProposalExecutedEvent = IWeb3DefaultEvent & {
  proposalId: Web3Uint256;
}

export type IProposalVotedEvent = IWeb3DefaultEvent & {
  voter: Web3Address;
  proposalId: Web3Uint256;
  support: number;
  weight: Web3Uint256;
  reason: string;
}

export interface IChilizConfig {
  chainId: number;
  deployerAddress: string;
  governanceAddress: string;
  parliaAddress: string;
}

export const TESTNET_CONFIG: IChilizConfig = {
  chainId: 17242,
  deployerAddress: '0x0000000000000000000000000000000000000010',
  governanceAddress: '0x0000000000000000000000000000000000000020',
  parliaAddress: '0x0000000000000000000000000000000000000030',
}

const DEPLOYER_ABI = require('../abi/Deployer.json')
const GOVERNANCE_ABI = require('../abi/Governance.json')
const PARLIA_ABI = require('../abi/Parlia.json')

export class ChilizStore {

  public isConnected: boolean = false

  private readonly keyProvider: IWeb3KeyProvider

  private deployerContract?: Contract;
  private governanceContract?: Contract;
  private parliaContract?: Contract;

  public constructor(private readonly config: IChilizConfig) {
    this.keyProvider = new Web3KeyProvider({
      expectedChainId: config.chainId,
    })
    makeAutoObservable(this)
  }

  public getKeyProvider(): IWeb3KeyProvider {
    return this.keyProvider
  }

  @action
  public async connectFromInjected() {
    this.isConnected = false
    if (!this.keyProvider.isConnected()) {
      await this.keyProvider.connectFromInjected()
    }
    this.deployerContract = this.keyProvider.createContract(DEPLOYER_ABI as any, this.config.deployerAddress) as any
    this.governanceContract = this.keyProvider.createContract(GOVERNANCE_ABI as any, this.config.governanceAddress) as any
    this.parliaContract = this.keyProvider.createContract(PARLIA_ABI as any, this.config.parliaAddress) as any
    this.isConnected = true
  }

  public async getVotingPowers(options: PastEventOptions = {}): Promise<any[]> {
    return await this.governanceContract!.getPastEvents('VotingPowerSet', options) as any
  }

  public async setVotingPower(voter: Web3Address, votingPower: Web3Uint256): Promise<IWeb3SendResult> {
    const data = this.governanceContract!.methods.setVotingPower(voter, votingPower).encodeABI()
    const account = this.keyProvider.currentAccount()
    return await this.keyProvider.sendTransactionAsync(account, this.config.governanceAddress, {
      data: data,
      gasLimit: '1000000',
      estimate: true,
    })
  }

  public async addDeployerProposal(deployer: Web3Address, description: string): Promise<IWeb3SendResult> {
    const abi = this.deployerContract!.methods.addDeployer(deployer).encodeABI()
    const data = this.governanceContract!.methods.propose(
      [this.config.deployerAddress],
      ['0x00'],
      [abi],
      description,
    ).encodeABI()
    const account = this.keyProvider.currentAccount()
    return await this.keyProvider.sendTransactionAsync(account, this.config.governanceAddress, {
      data: data,
      gasLimit: '1000000',
      estimate: true,
    })
  }

  public async voteForProposal(proposalId: Web3Uint256): Promise<IWeb3SendResult> {
    const data = this.governanceContract!.methods.castVote(proposalId, '1').encodeABI()
    const account = this.keyProvider.currentAccount()
    return await this.keyProvider.sendTransactionAsync(account, this.config.governanceAddress, {
      data: data,
      gasLimit: '1000000',
      estimate: true,
    })
  }

  public async voteAgainstProposal(proposalId: Web3Uint256): Promise<IWeb3SendResult> {
    const data = this.governanceContract!.methods.castVote(proposalId, '0').encodeABI()
    const account = this.keyProvider.currentAccount()
    return await this.keyProvider.sendTransactionAsync(account, this.config.governanceAddress, {
      data: data,
      gasLimit: '1000000',
      estimate: true,
    })
  }

  public async executeProposal(proposal: IProposalCreatedEvent): Promise<IWeb3SendResult> {
    const data = this.governanceContract!.methods.execute(
      proposal.targets,
      proposal.values,
      proposal.calldatas,
      keccak256(proposal.description),
    ).encodeABI()
    const account = this.keyProvider.currentAccount()
    return await this.keyProvider.sendTransactionAsync(account, this.config.governanceAddress, {
      data: data,
      gasLimit: '1000000',
      estimate: true,
    })
  }

  public matchProposalType(target: Web3Address, data: string): EProposalType {
    const config: Record<string, [Web3Address, string]> = {
      [EProposalType.AddDeployer]: ['0x0000000000000000000000000000000000000010', '0x880f4039']
    }
    for (const [type, [address, prefix]] of Object.entries(config)) {
      if (target === address && data.startsWith(prefix)) { // @ts-ignore
        return EProposalType[type.toString()]
      }
    }
    return EProposalType.Unknown
  }

  public async getProposalCreatedEvents(options: PastEventOptions = {}): Promise<IProposalCreatedEvent[]> {
    const result = await this.governanceContract!.getPastEvents('ProposalCreated', options) as any
    for (const r of result) {
      const {proposalId} = r.returnValues,
        state = await this.governanceContract!.methods.state(proposalId).call()
      r.status = EProposalStatus[Number(state)]
      console.log(`${proposalId} = ${state}`)
    }
    return result.map((log: any) => {
      return {...log, ...log.returnValues}
    })
  }

  public async getProposalCanceledEvents(options: PastEventOptions = {}): Promise<IProposalCanceledEvent[]> {
    return await this.governanceContract!.getPastEvents('ProposalCanceled', options) as any
  }

  public async getProposalExecutedEvents(options: PastEventOptions = {}): Promise<IProposalExecutedEvent[]> {
    return await this.governanceContract!.getPastEvents('ProposalExecuted', options) as any
  }

  public async getProposalVotedEvents(options: PastEventOptions = {}): Promise<IProposalVotedEvent[]> {
    return await this.governanceContract!.getPastEvents('VoteCast', options) as any
  }

  public async getDeployerAddedEvents(options: PastEventOptions = {}): Promise<any[]> {
    return await this.deployerContract!.getPastEvents('DeployerAdded', options) as any
  }

}