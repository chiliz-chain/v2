import {action, makeAutoObservable, observable} from "mobx";
import Web3Store from "./Web3Store";

import DEPLOYER_ABI from "../abi/DeployerV1.json"
import {Contract} from 'web3-eth-contract';

const deployerContractAddress = '0x0000000000000000000000000000000000000010';

export interface ISmartContract {
  blockNumber: number;
  transactionHash: string;
  account: string;
  contractAddress: string;
}

export default class DeployerStore {

  @observable
  deployers: string[] = [];
  @observable
  smartContracts: ISmartContract[] = [];

  @observable
  bannedDeployers: Set<string> = new Set<string>()

  @observable
  newDeployer: string = '';

  deployerContract: Contract | undefined;

  isLoading: boolean = false
  isAdding: boolean = false
  isRemoving: boolean = false
  isBanning: boolean = false

  constructor(private web3Store: Web3Store) {
    makeAutoObservable(this)
  }

  @action
  setNewDeployer(newDeployer: string) {
    this.newDeployer = newDeployer;
    console.log(this.newDeployer);
  }

  @action
  async connect(): Promise<void> {
    this.deployerContract = this.web3Store.createContract(DEPLOYER_ABI, deployerContractAddress)
    await this.loadDeployers();
    await this.loadSmartContracts();
  }

  @action
  async loadDeployers(): Promise<void> {
    if (!this.deployerContract) throw new Error(`Deployer is not initialized`);
    this.isLoading = true
    const possibleDeployers = await this.deployerContract.getPastEvents('DeployerAdded', {
      fromBlock: 'earliest'
    });
    const validDeployers = new Set<string>()
    for (const {returnValues: {account}} of possibleDeployers) {
      const isDeployer = await this.deployerContract.methods.isDeployer(account).call()
      if (isDeployer) {
        validDeployers.add(account)
      }
      const isBanned = await this.deployerContract.methods.isBanned(account).call()
      if (isBanned) {
        this.bannedDeployers.add(account)
      } else {
        this.bannedDeployers.delete(account)
      }
      console.log(`deployer ${account} isDeployer=${isDeployer}, isBanned=${isBanned}`)
    }
    this.deployers = Array.from(validDeployers);
    console.log(`Deployers: ${this.deployers}`);
    this.isLoading = false
  }

  @action
  async loadSmartContracts(): Promise<void> {
    if (!this.deployerContract) throw new Error(`Deployer is not initialized`);
    this.isLoading = true
    const deployedContracts = await this.deployerContract.getPastEvents('ContractDeployed', {
      fromBlock: 'earliest',
    })
    this.smartContracts = deployedContracts.map(({blockNumber, transactionHash, returnValues: {account, impl}}) => {
      return {blockNumber, transactionHash, account, contractAddress: impl}
    })
    console.log(this.smartContracts)
    this.isLoading = false
  }

  @action
  async addDeployer(deployer: string): Promise<void> {
    if (!this.deployerContract) throw new Error(`Deployer is not initialized`);
    this.isAdding = true
    const data = await this.deployerContract.methods.addDeployer(deployer)
      .encodeABI();
    const result = await this.web3Store.sendAsync(deployerContractAddress, {data, nonce: 0}),
      receipt = await result.receiptPromise
    console.log(`Deployer added: ${JSON.stringify(receipt, null, 2)}`);
    await this.loadDeployers()
    this.isAdding = false
  }

  @action
  async removeDeployer(deployer: string): Promise<void> {
    if (!this.deployerContract) throw new Error(`Deployer is not initialized`);
    this.isRemoving = true
    const data = await this.deployerContract.methods.removeDeployer(deployer)
      .encodeABI();
    const result = await this.web3Store.sendAsync(deployerContractAddress, {data, nonce: 0}),
      receipt = await result.receiptPromise
    console.log(`Deployer removed: ${JSON.stringify(receipt, null, 2)}`);
    await this.loadDeployers()
    this.isRemoving = false
  }

  @action
  async banDeployer(deployer: string): Promise<void> {
    if (!this.deployerContract) throw new Error(`Deployer is not initialized`);
    this.isBanning = true
    const data = await this.deployerContract.methods.banDeployer(deployer)
      .encodeABI();
    const result = await this.web3Store.sendAsync(deployerContractAddress, {data, nonce: 0}),
      receipt = await result.receiptPromise
    console.log(`Deployer removed: ${JSON.stringify(receipt, null, 2)}`);
    await this.loadDeployers()
    this.isBanning = false
  }
}
