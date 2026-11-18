// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// An agreement of two contracts, for the unit tests: World, the root, creates
// a Tally and decides who may join; the Tally counts, and remembers who
// counted last (msg.sender is the agent, also in a contract the root created).
contract Tally {
    uint256 public total;
    address public last;

    error TooMuch(uint256 n);

    function add(uint256 n) external returns (uint256) {
        if (n > 100) revert TooMuch(n);
        total += n;
        last = msg.sender;
        return total;
    }
}

contract World {
    Tally public tally;
    mapping(address => bool) public invited;

    constructor() {
        tally = new Tally();
        invited[msg.sender] = true;
    }

    function invite(address who) external {
        require(invited[msg.sender], "only the invited invite");
        invited[who] = true;
    }

    function approve_partner(address who) external view returns (bool) {
        return invited[who];
    }
}
