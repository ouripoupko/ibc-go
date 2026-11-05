// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// Test contract for the ibc EVM execution and state modules.
contract Registry {
    string public name;
    uint256 public counter;
    uint256 public lastTimestamp;
    address public lastSender;
    mapping(address => uint256) public values;

    constructor(string memory initialName, uint256 start) {
        name = initialName;
        counter = start;
    }

    /// Stores a value for msg.sender, bumps the counter and records block.timestamp.
    function setValue(uint256 value) external returns (uint256) {
        values[msg.sender] = value;
        counter += 1;
        lastTimestamp = block.timestamp;
        lastSender = msg.sender;
        return counter;
    }

    /// Writes state and then reverts, so every write must be rolled back.
    function setThenFail(uint256 value) external {
        values[msg.sender] = value;
        counter += 1;
        lastTimestamp = block.timestamp;
        revert("setThenFail: rejected");
    }

    /// View: the value stored for an address.
    function getValue(address who) external view returns (uint256) {
        return values[who];
    }

    /// View with several outputs.
    function info() external view returns (string memory, uint256, uint256, address) {
        return (name, counter, lastTimestamp, lastSender);
    }
}
