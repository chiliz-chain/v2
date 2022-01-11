import DeployerStore from "./stores/DeployerStore";
import Web3Store from "./stores/Web3Store";

const web3Store = new Web3Store();
const deployerStore = new DeployerStore(web3Store);

web3Store.connect().then(() => {
  // noinspection JSIgnoredPromiseFromCall
  deployerStore.connect();
});

export default {
  deployerStore,
  web3Store,
};
