**Proposal: Implementing a Burn Mechanism for 50% of All Transaction Fees on Chiliz Chain**

**Objective:**
The goal of this proposal is to introduce a burn mechanism on the Chiliz Chain to burn 50% of all transaction fees. This implementation aligns with the principles of Ethereum Improvement Proposal 1559 (EIP-1559) while maintaining compatibility with Ethereum's fee structure by setting the baseFee to 0 and burning 50% of the validator fees.

**Approach:**

1. **BaseFee Adjustment:**
   - Modify the Chiliz Chain client to set the baseFee to 0 for all transactions, aligning with Ethereum's fee structure.

2. **Validator Fee Burn Mechanism:**
   - Introduce a mechanism to burn 50% of the validator fees collected from transactions.

3. **Testing and Validation:**
   - Develop comprehensive test cases to validate the correctness and robustness of the implemented burn mechanism.

**Burning mechanism and other EVM-compatible chains:**
Binance Smart Chain (BSC) has had a similar burning mechanism in place prior to integrating EIP-1559. This [PR](https://github.com/bnb-chain/bsc/pull/1422) serves as an example of how the proposed burn mechanism aligns with the principles of EIP-1559 and demonstrates compatibility with other blockchain networks like BSC.

**Concerns:** 
It's important to acknowledge that some of the tests for Chiliz Chain's current implementation, such as `TestEIP1559Transition`, are not functioning as expected. While this proposal aims to fix some of these tests, it's possible that the modifications introduced may inadvertently break other tests or functionalities. Therefore, thorough testing and validation are essential to ensure the stability and reliability of the Chiliz Chain client after implementing the burn mechanism.

**Additional References:** 
For further insights into EIP-1559 and its implications for transaction fees, the following resources may be helpful:
- Blocknative's blog post on EIP-1559 fees, available [here](https://www.blocknative.com/blog/eip-1559-fees).
- The official Ethereum Improvement Proposal (EIP) document for EIP-1559, accessible [here](https://eips.ethereum.org/EIPS/eip-1559). This document provides detailed information on the rationale, specifications, and potential impacts of EIP-1559 on the Ethereum network.