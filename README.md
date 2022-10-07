Chiliz Chain 2.0
----------------

Chiliz Chain 2.0 will become a more open & interoperable successor of the current Chiliz Chain. The goal of Chiliz Chain 2.0 is to be the leading blockchain for the enterprise-level sports and entertainment brands who want to create a Web3 ecosystem where stakeholders can build Web3 experiences within a secure network-effect-driven community. Any developers interested in exploring the potential of Chiliz Fan Tokens have the chance to utilise the existing massive network of over 150+ leading sports IPs. As an EVM-compatible chain, Chiliz Chain 2.0 will stay compatible with the Ethereum tooling, making it simple and easy to build in the CC2.0 environment.

Chiliz Chain 2.0 is a fork of BSC (which is a go-ethereum fork). Hence, it is needless to say that most of the tooling mechanisms, concepts, binaries, and also the documentation  are hugely derived from the BSC and Ethereum. Using Geth as a CLI being one of them.

From that baseline of the EVM compatibility, Chiliz Chain 2.0 introduces a system of 11 validators with the Proof of Staked Authority (PoSA) consensus that supports shorter block time and lower fees. The most bonded validator candidates of staking then become validators and start producing blocks. Moreover, the double-sign detection and other slashing logic further guarantees security, stability, and the chain finality.

Chiliz Chain 2.0 in a nutshell is:

-   a successor of the Chiliz Chain.

-   a self-sovereign blockchain. It provides security and safety to the elected validators.

-   compatible with the EVM. It supports all the existing Ethereum tooling along with faster finality and reasonable transaction fees.

-   A distributed system with on-chain governance. Proof of Staked Authority brings in decentralization and community participants. As a native token, CHZ serves both; the gas of smart contract execution and tokens for staking.

-   Allows interoperability with Ethereum mainnet and other chains in the future.

Key features
------------

### Proof of Staked Authority

Although the Proof-of-Work (PoW) has been approved as a practical mechanism to implement a decentralized network, it is not friendly to the environment and requires a large size of participants to maintain the security.

Proof-of-Authority (PoA) provides some defense to 51% attack with an improved efficiency and tolerance to certain levels of Byzantine players (malicious or hacked). The POA protocol, on the other hand, is mostly criticized for not being as decentralized as PoW, since the validators have all the authority and are prone to corruption and security attacks.

Other blockchains, such as EOS and Cosmos both have introduced different types of Deputy Proof of Stake (DPoS) to allow token holders to vote and elect the validator set. It encourages decentralization and favors community governance.

To combine DPoS and PoA for consensus, Chiliz Chain 2.0 heavily inherits the following from the BSC consensus mechanism, Parlia:

1.  Blocks are produced by a limited set of validators.

2.  Validators take turns to produce blocks in a PoA manner, similar to the  Ethereum's Clique consensus engine.

3.  Validator sets are elected in and out based on the  staking governance on the Chiliz Chain.

4.  Parlia consensus engine interacts with a set of system contracts to achieve liveness slash, revenue distribution, and the validator set renewal function.

Native Token
------------

CHZ being the native token of CCv2 it will run on Chiliz Chain 2.0 the same way ETH runs on Ethereum. This means, CHZ will be used to:

1.  pay gas to deploy or invoke Smart Contract

2.  perform cross-chain operations, such as transfer token assets across Chiliz Chain 2.0 and Ethereum

3.  secure the network by staking/delegate it

Documentation 
--------------

Latest docs are here [link to our gitbook]

Contribution
------------

Your contribution to our reference material and source code means a lot. Thank you 😊

We welcome and encourage contributions from the brightest minds like yourself and will be grateful to even the smallest of fixes you'd suggest.

So, it's time to mark your contribution to the CCv2 in 4 easy steps:

Fork > Fix > Commit > Send a pull request

That's it. The reviewers will take it from there.

If you want to go an extra mile and offer solutions or recommendations on complex topics, we recommend you to please contact our core development team on our Discord channel. This is to ensure your offered help is aligned with the objective, demand, and philosophy of the project. 

By following this process, we can assure the least time and efforts are invested in taking any such change forward. That being said, we offer coding instructions and request you to follow while submitting your valuable suggestions:

1.  Your code must adhere to the official Go formatting guidelines (i.e. gofmt).

2.  Stick to the official Go commentary guidelines while writing your code.

3.  Ensure your pull requests are based on and opened against the Master branch.

4.  Ensure to enter prefixes of the packages you intend to modify in your commit messages. For example, "eth, rpc: make trace configs optional"

See the Developer Guide to get more insights on the topics such as Environment set up, Project management, and further testing methodologies. 

License
-------

The CC2 library (i.e. all code outside of the cmd directory) is licensed under the [GNU Lesser General Public License v3.0](https://www.gnu.org/licenses/lgpl-3.0.en.html), also included in our repository in the COPYING.LESSER file.

The CC2 binaries (i.e. all code inside of the cmd directory) is licensed under the [GNU General Public License v3.0](https://www.gnu.org/licenses/gpl-3.0.en.html), also included in our repository in the COPYING file.