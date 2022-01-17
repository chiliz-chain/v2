// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "@openzeppelin/contracts/access/Ownable.sol";
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
}

interface IVersional {

    function getVersion() external pure returns (uint256);
}

interface IInjector {

    function getDeployer() external view returns (IDeployer);

    function getGovernance() external view returns (IGovernance);

    function getParlia() external view returns (IParlia);
}

abstract contract OnlyInit {

    bool private _init;

    modifier whenNotInitialized() {
        require(!_init, "OnlyInit: already initialized");
        _;
        _init = true;
    }

    modifier whenInitialized() {
        require(_init, "OnlyInit: not initialized yet");
        _;
    }
}

abstract contract InjectorContextHolder is IInjector, IVersional, Ownable, OnlyInit {

    IDeployer private _deployer;
    IGovernance private _governance;
    IParlia private _parlia;

    function init() public whenNotInitialized {
        _deployer = IDeployer(0x0000000000000000000000000000000000000010);
        _governance = IGovernance(0x0000000000000000000000000000000000000020);
        _parlia = IParlia(0x0000000000000000000000000000000000000030);
    }

    function init(
        IDeployer deployer,
        IGovernance governance,
        IParlia parlia
    ) public whenNotInitialized {
        _deployer = deployer;
        _governance = governance;
        _parlia = parlia;
    }

    modifier onlyBlockchain() {
        require(msg.sender == address(0x00), "InjectorContextHolder: only blockchain");
        _;
    }

    modifier onlyCoinbase() {
        require(msg.sender == block.coinbase, "InjectorContextHolder: only coinbase");
        _;
    }

    function getDeployer() public view whenInitialized override returns (IDeployer) {
        return _deployer;
    }

    modifier onlyDeployer() {
        require(getDeployer().isDeployer(msg.sender), "InjectorContextHolder: only deployer");
        _;
    }

    function getGovernance() public view whenInitialized override returns (IGovernance) {
        return _governance;
    }

    modifier onlyGovernance() {
        require(IGovernance(msg.sender) == getGovernance(), "InjectorContextHolder: only governance");
        _;
    }

    function getParlia() public view whenInitialized override returns (IParlia) {
        return _parlia;
    }

    modifier onlyValidator() {
        require(getParlia().isValidator(msg.sender), "InjectorContextHolder: only parlia");
        _;
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
