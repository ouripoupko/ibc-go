// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

// Calls the EVM's precompiled contracts, for the unit tests: Solidity's own
// sha256, ripemd160 and ecrecover, and any precompile by its address.
contract Precompiles {
    function hashSha256(bytes calldata data) external pure returns (bytes32) {
        return sha256(data);
    }

    function hashRipemd160(bytes calldata data) external pure returns (bytes20) {
        return ripemd160(data);
    }

    function recover(bytes32 digest, uint8 v, bytes32 r, bytes32 s) external pure returns (address) {
        return ecrecover(digest, v, r, s);
    }

    // run calls the precompile at precompile with input, and returns its
    // output; it reverts if the precompile fails.
    function run(address precompile, bytes calldata input) external view returns (bytes memory) {
        (bool ok, bytes memory out) = precompile.staticcall(input);
        require(ok, "precompile failed");
        return out;
    }
}
