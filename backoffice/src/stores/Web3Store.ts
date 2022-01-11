import {action} from "mobx";
import Web3 from "web3";
import detectEthereumProvider from '@metamask/detect-provider';
import {Contract} from "web3-eth-contract";
import {PromiEvent, TransactionReceipt} from 'web3-core';
import {numberToHex} from "web3-utils";
import {Transaction} from 'ethereumjs-tx';

export interface ISendOptions {
  data?: string;
  gasLimit?: string;
  value?: string;
  nonce?: number;
}

export interface ISendAsyncResult {
  receiptPromise: PromiEvent<TransactionReceipt>;
  transactionHash: string;
  rawTransaction: string;
}

export default class Web3Store {

  private web3: Web3 | undefined;

  public getWeb3(): Web3 {
    if (!this.web3) throw new Error(`Web3 is not initialized`);
    return this.web3;
  }

  @action
  async connect(): Promise<void> {
    const provider = await detectEthereumProvider();
    if (!provider) {
      throw new Error(`MetaMask is not installed`);
    }
    (provider as any)
      .request({method: 'eth_requestAccounts'})
      .then((accounts: any) => {
        console.log(`Accounts: ${accounts}`);
      })
      .catch((err: any) => {
        if (err.code === 4001) {
          // EIP-1193 userRejectedRequest error
          // If this happens, the user rejected the connection request.
          console.log('Please connect to MetaMask.');
        } else {
          console.error(err);
        }
      });
    this.web3 = new Web3(provider as any);
  }

  @action
  public async sendAsync(
    to: string,
    sendOptions: ISendOptions,
  ): Promise<ISendAsyncResult> {
    const [from] = await this.getWeb3().eth.getAccounts();
    let gasPrice = await this.getWeb3().eth.getGasPrice();
    if (Number(gasPrice) < 20_000000000) {
      gasPrice = String(20_000000000);
    }
    console.log('Gas Price: ' + gasPrice);
    let nonce = sendOptions.nonce;
    if (!nonce) {
      nonce = await this.getWeb3().eth.getTransactionCount(from);
      console.log('Nonce: ' + nonce);
    }
    const chainId = await this.getWeb3().eth.getChainId();
    console.log('ChainID: ' + chainId);
    const tx = {
      from: from,
      to: to,
      value: numberToHex(sendOptions.value || '0'),
      gas: numberToHex(sendOptions.gasLimit || '500000'),
      gasPrice: gasPrice,
      data: sendOptions.data,
      nonce: nonce,
      chainId: chainId,
    };
    console.log('Sending transaction via Web3: ', tx);
    return new Promise<ISendAsyncResult>((resolve, reject) => {
      const promise = this.getWeb3().eth.sendTransaction(tx);
      promise
        .once('transactionHash', async (transactionHash: string) => {
          console.log(`Just signed transaction has is: ${transactionHash}`);
          const rawTx = await this.getWeb3().eth.getTransaction(
            transactionHash,
          );
          console.log(
            `Found transaction in node: `,
            JSON.stringify(rawTx, null, 2),
          );
          const rawTxHex = this.tryGetRawTx(chainId, rawTx);
          resolve({
            receiptPromise: promise,
            transactionHash: transactionHash,
            rawTransaction: rawTxHex,
          });
        })
        .catch(reject);
    });
  }

  private tryGetRawTx(chainId: number, rawTx: any): string {
    const allowedChains = ['1', '3', '4', '42', '5'];
    if (!allowedChains.includes(`${chainId}`)) {
      console.warn(`raw tx can't be greater for this chain id ${chainId}`);
      return '';
    }
    const {v, r, s} = rawTx as any; /* this fields are not-documented */
    const newTx = new Transaction(
      {
        gasLimit: this.getWeb3().utils.numberToHex(rawTx.gas),
        gasPrice: this.getWeb3().utils.numberToHex(Number(rawTx.gasPrice)),
        to: `${rawTx.to}`,
        nonce: this.getWeb3().utils.numberToHex(rawTx.nonce),
        data: rawTx.input,
        v: v,
        r: r,
        s: s,
        value: this.getWeb3().utils.numberToHex(rawTx.value),
      },
      {
        chain: chainId,
      },
    );
    if (!newTx.verifySignature())
      throw new Error(`The signature is not valid for this transaction`);
    console.log(`New Tx: `, JSON.stringify(newTx, null, 2));
    const rawTxHex = newTx.serialize().toString('hex');
    console.log(`Raw transaction hex is: `, rawTxHex);
    return rawTxHex;
  }

  public createContract(abi: any, address: string): Contract {
    if (!this.web3) throw new Error('Web3 must be initialized');
    return new this.web3.eth.Contract(abi, address);
  }

}
