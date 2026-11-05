// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// The smallest useful ibc contract: shared state, msg.sender, a revert
/// reason, and an owner-only function.
contract Counter {
    address public owner;
    uint256 public count;
    mapping(address => uint256) public contributions;

    constructor(uint256 start) {
        owner = msg.sender; // the agent that deployed the contract
        count = start;
    }

    /// Adds amount to the counter and returns the new value.
    function increment(uint256 amount) external returns (uint256) {
        require(amount > 0, "amount must be positive");
        count += amount;
        contributions[msg.sender] += amount;
        return count;
    }

    /// Only the owner may reset the counter.
    function reset() external {
        require(msg.sender == owner, "only the owner can reset");
        count = 0;
    }

    /// What the calling agent has added so far.
    function myContribution() external view returns (uint256) {
        return contributions[msg.sender];
    }
}
