import {MobXProviderContext} from 'mobx-react'
import React from "react";
import {ChilizStore, TESTNET_CONFIG} from "./ChilizStore";

let currentEnvironment = '${REACT_APP_ENVIRONMENT}'
if (currentEnvironment === '${REACT_APP_ENVIRONMENT}') {
  currentEnvironment = `${process.env.REACT_APP_ENVIRONMENT}`
}
if (!currentEnvironment) {
  currentEnvironment = 'staging'
}
console.log(`Current env is: ${currentEnvironment}`)

const chilizStore = new ChilizStore(TESTNET_CONFIG)
chilizStore.connectFromInjected().then(async () => {
  const currentAccount = chilizStore.getKeyProvider().currentAccount()
  console.log(`Current account is: ${currentAccount}`)
})

export const useStores = () => {
  return React.useContext(MobXProviderContext)
}

export const useChilizStore = (): ChilizStore => chilizStore