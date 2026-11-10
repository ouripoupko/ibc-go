# noble-hashes 1.3.2 (vendored)

The keccak-256 of [@noble/hashes](https://github.com/paulmillr/noble-hashes) 1.3.2, MIT license
(see `LICENSE`): the ES module files `sha3.js` and the files it imports, copied unchanged from the
npm package except for two edits, so that browsers load them without a bundler:

- `utils.js` imports `./crypto.js` instead of `@noble/hashes/crypto`;
- the `sourceMappingURL` comments are removed (the maps are not copied).

The ibc client package uses it for agent addresses and EIP-712 digests; the build copies it to
`dist/vendor` unchanged.
