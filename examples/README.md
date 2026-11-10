# Examples

Example contracts and clients for ibc. The [usage guide](../docs/usage.md) walks through them.

## Contracts

| Contract | Shows |
|---|---|
| [`Counter.sol`](contracts/Counter.sol) | State, `msg.sender`, a revert reason, an owner-only function |
| [`Poll.sol`](contracts/Poll.sol) | Array arguments, several return values, custom errors |
| [`Board.sol`](contracts/Board.sol) | Structs returned as JSON objects, strings, `block.timestamp`, `whoAmI()` |
| [`Chat.sol`](contracts/Chat.sol) | A contract for an app: users let it post and like without asking; the topic asks each time. Anyone may join as a guest (`approve_guest`) |

The compiled contracts are in [`build/`](build): `<Name>.bin` (bytecode) and `<Name>.abi`
(interface). After changing a contract, compile it again (needs Node.js):

```sh
examples/scripts/compile.sh               # all contracts
examples/scripts/compile.sh Counter.sol   # one contract
```

`examples_test.go` deploys every contract on a test node and checks the results the usage guide
shows. It runs with `go test ./...`.

## Clients

| Client | Signs with | Needs |
|---|---|---|
| [`scripts/ibc.sh`](scripts/ibc.sh) | A key file, through the `ibc` program | bash, curl, jq, `ibc` |
| [`js/demo.mjs`](js/demo.mjs) | The client package [`@ouripoupko/ibc`](../js) (a passkey in a browser, a WebCrypto key here); apps go through the user's node | Node.js 20+ |

From the shell, with a node running at `http://localhost:2611` and the `ibc` program built:

```sh
export IBC_KEY=~/.ibc/alice.key IBC_BIN=./ibc
examples/scripts/ibc.sh newkey
examples/scripts/ibc.sh register
C=$(examples/scripts/ibc.sh deploy Counter '{"start": 10}')
examples/scripts/ibc.sh write "$C" increment '{"amount": 5}'   # 15
examples/scripts/ibc.sh read "$C" count                        # 15
```

From JavaScript, [`js/demo.mjs`](js/demo.mjs) deploys and calls all three contracts, with a
WebCrypto key standing in for the passkey. It uses the client package from [`js/`](../js):

```sh
cd examples/js
npm install                         # links ../../js
node demo.mjs http://localhost:2611
```
