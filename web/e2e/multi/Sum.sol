// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// A library in its own file, which Tally.sol imports (the browser test's
// multi-file deploy).
library Sum {
    function add(uint256 a, uint256 b) internal pure returns (uint256) {
        return a + b;
    }
}
