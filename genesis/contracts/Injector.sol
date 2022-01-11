// SPDX-License-Identifier: GPL-3.0-only
pragma solidity ^0.8.0;

import "@openzeppelin/contracts/access/Ownable.sol";
import "@openzeppelin/contracts/utils/Address.sol";

interface IDeployer {

    function isDeployer(address account) external view returns (bool);

    function isBanned(address account) external view returns (bool);

    function addDeployer(address account) external;

    function banDeployer(address account) external;

    function unbanDeployer(address account) external;

    function removeDeployer(address account) external;

    function getContractDeployer(address impl) external view returns (address);
}

interface IEVMHooks {

    function registerDeployedContract(address account, address impl) external;

    function checkContractActive(address impl) external;
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

    modifier onlyInit() {
        require(!_init, "OnlyInit: already initialized");
        _;
        _init = true;
    }

    function requireInit() internal view {
        require(_init, "OnlyInit: not initialized yet");
    }
}

abstract contract InjectorContextHolder is IInjector, IVersional, Ownable, OnlyInit {

    IDeployer private _deployer;
    IGovernance private _governance;
    IParlia private _parlia;

    function initialize(
        IDeployer deployer,
        IGovernance governance,
        IParlia parlia
    ) public onlyInit {
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

    function getDeployer() public view override returns (IDeployer) {
        requireInit();
        return _deployer;
    }

    function getGovernance() public view override returns (IGovernance) {
        requireInit();
        return _governance;
    }

    function getParlia() public view override returns (IParlia) {
        requireInit();
        return _parlia;
    }

    modifier onlyGovernance() {
        require(IGovernance(msg.sender) == getGovernance(), "InjectorContextHolder: only governance");
        _;
    }

    modifier onlyDeployer() {
        require(getDeployer().isDeployer(msg.sender), "InjectorContextHolder: only deployer");
        _;
    }

    modifier onlyValidator() {
        require(getParlia().isValidator(msg.sender), "InjectorContextHolder: only validator");
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
