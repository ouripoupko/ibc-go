// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

/// Test contract with membership policies: the owner invites agents, only
/// invited agents may join, and only the owner may remove others.
contract Club {
    address public owner;
    mapping(address => bool) public invited;

    constructor() {
        owner = msg.sender;
    }

    function invite(address who) external {
        require(msg.sender == owner, "only the owner invites");
        invited[who] = true;
    }

    /// Called by ibc, as the joining agent, when it asks to join.
    function approve_partner(address partner) external view returns (bool) {
        return invited[partner];
    }

    /// Called by ibc, as the requesting member, to remove another member.
    function approve_removal(address member) external view returns (bool) {
        return msg.sender == owner && member != owner;
    }
}
