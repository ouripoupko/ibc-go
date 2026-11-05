# Examples

Example contracts and clients for ibc. The [usage guide](../docs/usage.md) walks through them.

## Contracts

| Contract | Shows |
|---|---|
| [`Counter.sol`](contracts/Counter.sol) | State, `msg.sender`, a revert reason, an owner-only function |
| [`Poll.sol`](contracts/Poll.sol) | Array arguments, several return values, custom errors |
| [`Board.sol`](contracts/Board.sol) | Structs returned as JSON objects, strings, `block.timestamp`, `whoAmI()` |

The compiled contracts are in [`build/`](build): `<Name>.bin` (bytecode) and `<Name>.abi`
(interface). After changing a contract, compile it again (needs Node.js):

```sh
examples/scripts/compile.sh               # all contracts
examples/scripts/compile.sh Counter.sol   # one contract
```

`examples_test.go` deploys every contract on a test node and checks the results the usage guide
shows. It runs with `go test ./...`.

## Clients

| Client | Needs |
|---|---|
| [`scripts/ibc.sh`](scripts/ibc.sh) | bash, curl, jq |
| [`js/ibc.mjs`](js/ibc.mjs) | A browser, or Node.js 18+ |

From the shell, with a node running at `http://localhost:2611`:

```sh
export IBC_AGENT=alice
examples/scripts/ibc.sh register
C=$(examples/scripts/ibc.sh deploy Counter '{"start": 10}')
examples/scripts/ibc.sh write "$C" increment '{"amount": 5}'   # 15
examples/scripts/ibc.sh read "$C" count                        # 15
```

From JavaScript, [`js/demo.mjs`](js/demo.mjs) deploys and calls all three contracts:

```sh
node examples/js/demo.mjs http://localhost:2611 alice
```
