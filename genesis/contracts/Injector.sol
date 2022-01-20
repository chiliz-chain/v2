// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "@openzeppelin/contracts/utils/Address.sol";

interface IEvmHooks {

    function registerDeployedContract(address account, address impl) external;

    function checkContractActive(address impl) external;
}

interface IDeployer is IEvmHooks {

    function isDeployer(address account) external view returns (bool);

    function isBanned(address account) external view returns (bool);

    function addDeployer(address account) external;

    function banDeployer(address account) external;

    function unbanDeployer(address account) external;

    function removeDeployer(address account) external;

    function getContractDeployer(address impl) external view returns (address);
}

interface IGovernance {
}

interface IParlia {

    function isValidator(address account) external view returns (bool);

    function addValidator(address account) external;

    function removeValidator(address account) external;

    function getValidators() external view returns (address[] memory);

    function deposit(address validator) external payable;

    function claimDepositFee(address payable validator) external;

    function slash(address validator) external;
}

interface IVersional {

    function getVersion() external pure returns (uint256);
}

interface IInjector {

    function getDeployer() external view returns (IDeployer);

    function getGovernance() external view returns (IGovernance);

    function getParlia() external view returns (IParlia);
}

abstract contract InjectorContextHolder is IInjector, IVersional {

    bool private _init; // 0

    modifier whenNotInitialized() {
        require(!_init, "OnlyInit: already initialized");
        _;
        _init = true;
    }

    modifier whenInitialized() {
        require(_init, "OnlyInit: not initialized yet");
        _;
    }

    IDeployer private _deployer; // 1
    IGovernance private _governance; // 2
    IParlia private _parlia; // 3

    uint256[50-4] private _gap;

    function init() public whenNotInitialized virtual {
        _deployer = IDeployer(0x0000000000000000000000000000000000000010);
        _governance = IGovernance(0x0000000000000000000000000000000000000020);
        _parlia = IParlia(0x0000000000000000000000000000000000000030);
    }

    function initManually(IDeployer deployer, IGovernance governance, IParlia parlia) public whenNotInitialized {
        _deployer = deployer;
        _governance = governance;
        _parlia = parlia;
    }

    modifier onlyFromCoinbaseOrGovernance() {
        require(msg.sender == block.coinbase || IGovernance(msg.sender) == getGovernance(), "InjectorContextHolder: only coinbase or governance");
        _;
    }

    modifier onlyFromGovernance() {
        require(IGovernance(msg.sender) == getGovernance(), "InjectorContextHolder: only governance");
        _;
    }

    function getDeployer() public view whenInitialized override returns (IDeployer) {
        return _deployer;
    }

    function getGovernance() public view whenInitialized override returns (IGovernance) {
        return _governance;
    }

    function getParlia() public view whenInitialized override returns (IParlia) {
        return _parlia;
    }

    function isV1Compatible() public virtual pure returns (bool);
}

abstract contract InjectorContextHolderV1 is InjectorContextHolder {

    function getVersion() public pure virtual override returns (uint256) {
        return 0x01;
    }

    function isV1Compatible() public pure override returns (bool) {
        return getVersion() >= 0x01;
    }
}
