// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

import "./Sum.sol";

contract Tally {
    uint256 public total;

    function count(uint256 n) external returns (uint256) {
        total = Sum.add(total, n);
        return total;
    }
}
