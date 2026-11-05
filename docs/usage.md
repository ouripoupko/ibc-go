# Usage guide

This guide takes you from a running node to a deployed contract you can call. It assumes a node is
running at `http://localhost:2611`; the [README](../README.md) shows how to start one. Every call
used here is described in full in the [API reference](api.md).

- [Concepts](#concepts)
- [Write a contract](#write-a-contract)
- [Compile it](#compile-it)
- [Call ibc from the shell](#call-ibc-from-the-shell)
- [The same calls with curl](#the-same-calls-with-curl)
- [Call ibc from JavaScript](#call-ibc-from-javascript)
- [The example contracts](#the-example-contracts)
- [Joining another agent's contract](#joining-another-agents-contract)
- [Common errors](#common-errors)

## Concepts

- **Agent.** Every request acts as an agent, identified by a string you choose, such as a UUID or
  a user name. An agent is registered on a node once, with the address at which other nodes can
  reach it.
- **Contract.** A program you deploy. Deploying returns the contract's id, which every later call
  uses. Each contract has its own state, separate from every other contract.
- **Partners.** The agents who share a contract. Each partner's node keeps its own copy of the
  contract.
- **Writes and reads.** A *write* (`contract_write`) is a transaction: it can change the
  contract's state, it is recorded in the contract's ledger, and its result arrives later on the
  event stream. A *read* (`contract_read`) changes nothing and answers at once.
- **Event stream.** A long-lived HTTP connection (`/stream`) on which the node reports the outcome
  of every deploy, write and join. Open it before you send a write, so you do not miss the result.

## Write a contract

Contracts are ordinary Solidity. Here is the smallest of the [examples](../examples/contracts):

```solidity
// SPDX-License-Identifier: MIT
pragma solidity ^0.8.24;

contract Counter {
    address public owner;
    uint256 public count;
    mapping(address => uint256) public contributions;

    constructor(uint256 start) {
        owner = msg.sender; // the agent that deployed the contract
        count = start;
    }

    function increment(uint256 amount) external returns (uint256) {
        require(amount > 0, "amount must be positive");
        count += amount;
        contributions[msg.sender] += amount;
        return count;
    }

    function reset() external {
        require(msg.sender == owner, "only the owner can reset");
        count = 0;
    }

    function myContribution() external view returns (uint256) {
        return contributions[msg.sender];
    }
}
```

### What is different on ibc

| In Solidity | On ibc |
|---|---|
| `msg.sender`, `tx.origin` | The calling agent. Its address is `keccak256(agent id)`, last 20 bytes. Every agent always has the same address |
| `block.timestamp` | The time the node received the request, in seconds |
| `block.number` | How many records the contract has processed |
| Return values | A write's return value arrives on the event stream; a read's comes back in the HTTP reply |
| `require` / `revert` messages, custom errors | Become the reply, for example `execution reverted: amount must be positive` or `execution reverted: AlreadyVoted(1)` |
| `msg.value`, `payable`, sending ether | There is no ether. `msg.value` is always 0, and sending any amount fails |
| `emit` | Allowed, but events are not reported anywhere yet |
| `keccak256` | Works |
| `sha256` | Reverts: ibc has no precompiled contracts |
| `ecrecover`, `ripemd160` | **Return zero without failing.** Do not use them |
| `block.coinbase`, `block.prevrandao`, `block.basefee`, `blockhash` | Always zero |
| Gas | Each call may use up to 100 million gas. Nobody pays for it; it only stops runaway code |

Every partner runs the same calls on the same inputs and must reach the same state, so a contract
cannot see anything that differs between nodes. That is why the fields above are fixed.

Solidity features up to the Osaka fork work, so recent compilers can be used with their default
settings.

### Tips

- **Return what the client needs.** A write's return value is the only result the client gets
  back from it, so return the new id or the new value.
- **Use `view` functions for reads.** Any function can be called with `contract_read`, but only a
  `view` function is guaranteed not to try to change anything. With `contract_read`, nothing
  changes anyway: the call runs on a copy of the state, which is then thrown away.
- **Use `msg.sender` for "me".** An app usually does not know an agent's address. A function like
  `myVote()` or `whoAmI()` lets the agent ask the contract.
- **Name your parameters.** Arguments are passed by the names in the ABI, so a parameter called
  `amount` is passed as `{"amount": 5}`.

## Compile it

The examples come with a script that compiles them with solc. It needs Node.js, and downloads the
compiler the first time:

```sh
examples/scripts/compile.sh               # every contract in examples/contracts
examples/scripts/compile.sh Counter.sol   # one contract
```

Each contract produces two files in `examples/build`:

- `Counter.bin`: the bytecode, as hex text.
- `Counter.abi`: the interface, as JSON. ibc uses it to encode your arguments and decode the
  results.

To compile your own contract anywhere else, run solc yourself:

```sh
npx -p solc@0.8.38 solcjs --bin --abi --optimize -o build MyContract.sol
```

solcjs names its output files after the source path and the contract, for example
`MyContract_sol_MyContract.bin`. A native `solc --bin --abi` works just as well.

## Call ibc from the shell

[`examples/scripts/ibc.sh`](../examples/scripts/ibc.sh) is a small client built on `curl` and
`jq`. It waits for each write's result on the event stream and prints just the result.

```sh
export IBC_URL=http://localhost:2611   # the node (this is the default)
export IBC_AGENT=alice                 # who you are
alias ibc=examples/scripts/ibc.sh

ibc register
# registered alice at http://localhost:2611

C=$(ibc deploy Counter '{"start": 10}')       # deploys build/Counter.bin, prints the contract id

ibc write "$C" increment '{"amount": 5}'
# 15
ibc write "$C" increment '{"amount": 0}'
# "execution reverted: amount must be positive"

ibc read "$C" count
# 15
ibc read "$C" myContribution
# 5
ibc read "$C" owner
# "0x5dad7600C5D89fE3824fFa99ec1c3eB8BF3b0501"
```

Other commands:

```sh
ibc exists                 # is IBC_AGENT registered on the node?
ibc contracts              # the agent's contracts
ibc contract "$C"          # one contract's details
ibc ledger "$C"            # the records the contract has processed
ibc stream                 # follow every event of the agent (Ctrl-C to stop)
```

Run `examples/scripts/ibc.sh` without arguments for the full list.

## The same calls with curl

Without the script, every call is one HTTP request. Open the event stream in one terminal:

```sh
curl -sN 'http://localhost:2611/stream?agent=alice&contract='
```

and send the requests from another:

```sh
IBC=http://localhost:2611

# Register the agent, once per node.
curl -s -X PUT "$IBC/ibc/app/alice?action=register_agent" \
  -H 'Content-Type: application/json' -d "{\"address\": \"$IBC\"}"

# Build the deploy message: bytecode, ABI, and constructor arguments by name.
jq -n --arg address "$IBC" \
  --arg code "$(cat examples/build/Counter.bin)" \
  --rawfile abi examples/build/Counter.abi \
  '{name: "my-counter", contract: "Counter.sol", protocol: "BFT", default_app: "",
    pid: "alice", address: $address, profile: "",
    code: $code, abi: $abi, constructor: {start: 10}}' > deploy.json

# Deploy. The reply is the new contract's id.
CONTRACT=$(curl -s -X PUT "$IBC/ibc/app/alice?action=deploy_contract" \
  -H 'Content-Type: application/json' -d @deploy.json | jq -r .)

# A write. The reply is a request id; the result appears on the event stream.
curl -s -X POST "$IBC/ibc/app/alice/$CONTRACT/increment?action=contract_write" \
  -H 'Content-Type: application/json' -d '{"values": {"amount": 5}}'

# A read. The reply is the result.
curl -s -X POST "$IBC/ibc/app/alice/$CONTRACT/count?action=contract_read" \
  -H 'Content-Type: application/json' -d '{"values": {}}'
```

The URL always has the form `/ibc/app/<agent>/<contract>/<method>?action=<action>`. The agent in
the URL is the caller: it becomes `msg.sender`.

On the stream, each result is one line:

```
data: {"request":"f64c…","contract":null,"action":"register_agent","reply":true,"agent":"alice"}
data: {"request":"520f…","contract":"520f…","action":"deploy_contract","reply":true,"state_root":"0x…","agent":"alice"}
data: {"request":"c690…","contract":"520f…","action":"contract_write","reply":15,"state_root":"0x…","agent":"alice"}
```

Match `request` with the id you got when you sent the request. Empty `data:` lines are keep-alives.

## Call ibc from JavaScript

[`examples/js/ibc.mjs`](../examples/js/ibc.mjs) is a small client without dependencies. It works
in browsers and in Node.js 18 and later. It opens the event stream for you and resolves each write
with its result:

```js
import { Ibc } from './ibc.mjs';

const ibc = new Ibc('http://localhost:2611', 'alice');
await ibc.register();

const code = await (await fetch('/build/Counter.bin')).text();
const abi = await (await fetch('/build/Counter.abi')).text();
const counter = await ibc.deploy({ name: 'my-counter', code, abi, constructor: { start: 10 } });

await ibc.write(counter, 'increment', { amount: 5 });   // 15
await ibc.read(counter, 'count');                       // 15

try {
  await ibc.write(counter, 'increment', { amount: 0 });
} catch (err) {
  console.log(err.message);   // execution reverted: amount must be positive
}

ibc.close();
```

[`examples/js/demo.mjs`](../examples/js/demo.mjs) deploys and calls all three examples from
Node.js:

```sh
node examples/js/demo.mjs http://localhost:2611 alice
```

If you would rather write your own client, the pattern is in the
[API reference](api.md#the-event-stream): open the stream, send the request, and wait for the
event whose `request` matches the id you got back.

> **Large numbers in JavaScript.** Results are exact JSON numbers. `JSON.parse` keeps integers
> exactly only up to 2^53. For values that can grow beyond that, such as token amounts in wei,
> return them as `string` from the contract, or parse the reply with a big-number-aware parser.
> When you *send* a large integer, pass it as a string: `{"amount": "1000000000000000000000"}`.

## The example contracts

| Contract | Shows |
|---|---|
| [Counter](../examples/contracts/Counter.sol) | State, `msg.sender`, a revert reason, an owner-only function |
| [Poll](../examples/contracts/Poll.sol) | Array arguments, several return values, custom errors |
| [Board](../examples/contracts/Board.sol) | Structs returned as JSON objects, strings, `block.timestamp`, `whoAmI()` |

### Poll

```sh
P=$(ibc deploy Poll '{"text": "Lunch?", "choices": ["pizza", "salad", "soup"]}')

ibc write "$P" vote '{"option": 1}'
# 1
ibc write "$P" vote '{"option": 2}'
# "execution reverted: AlreadyVoted(1)"
ibc write "$P" vote '{"option": 7}'
# "execution reverted: NoSuchOption(7, 3)"

ibc read "$P" results
# ["Lunch?",["pizza","salad","soup"],[0,1,0]]
ibc read "$P" myVote
# [true,1]
```

A function with several return values answers with a JSON array. Custom errors are reported with
their name and arguments.

A deployment can fail too. The constructor requires at least two options:

```sh
ibc deploy Poll '{"text": "Lunch?", "choices": ["pizza"]}'
# ibc.sh: deploy failed: execution reverted: a poll needs at least two options
```

### Board

```sh
B=$(ibc deploy Board)

ibc write "$B" post '{"text": "hello ibc"}'
# 0
ibc read "$B" getPost '{"id": 0}'
# {"author":"0x5dad7600C5D89fE3824fFa99ec1c3eB8BF3b0501","text":"hello ibc","time":1793900673}
ibc read "$B" latest '{"limit": 5}'
# [{"author":"0x5dad…","text":"hello ibc","time":1793900673}]
ibc read "$B" whoAmI
# "0x5dad7600C5D89fE3824fFa99ec1c3eB8BF3b0501"
```

A struct comes back as a JSON object keyed by its field names, and an array of structs as an array
of objects.

### Arguments

Pass arguments as a JSON object keyed by the parameter names in the ABI:

| Solidity type | Send | Receive |
|---|---|---|
| `uint256`, `int64`, … | `5`, or a string: `"5"`, `"0x05"` | `5` |
| `bool` | `true` | `true` |
| `string` | `"hello"` | `"hello"` |
| `address` | `"0x5dad7600C5D89fE3824fFa99ec1c3eB8BF3b0501"` | the same, checksummed |
| `bytes`, `bytes32` | `"0x00ff…"` | `"0x00ff…"` |
| `string[]`, `uint256[3]` | `["a", "b"]` | `["a", "b"]` |
| struct | `{"author": "0x…", "text": "hi"}` | the same |

A parameter without a name, such as the key of a `public` mapping's getter, is passed by its
position: `ibc read "$C" contributions '{"0": "0x5dad…"}'`.

## Joining another agent's contract

An agent joins a contract by asking one of its partners, on the partner's node:

```sh
IBC_AGENT=bob ibc register
IBC_AGENT=bob ibc join "$C" http://localhost:2611 alice
```

The partner's node answers on bob's event stream with an `a2a_reply_join` event.

> **Not available yet.** Joins are never approved in this version (`approve_partner` is not
> defined for Solidity contracts yet), and partners do not exchange writes until the consensus
> protocol is written. For now, only the agent that deployed a contract can use it. Calls by any
> other agent are answered with `unknown contract`.

## Common errors

| Reply | Meaning |
|---|---|
| `{"message": "unknown agent \"alice\""}` | Register the agent first |
| `{"message": "unknown contract \"…\" of agent \"bob\""}` | This agent does not have the contract on this node. Check the id, and that the agent in the URL is the one who deployed it |
| `{"message": "no such contract"}` | A read of a contract that does not exist, or whose deployment has not finished yet |
| `"increment: missing argument \"amount\""` | A key in `values` is missing or misspelled. Names must match the ABI exactly |
| `"increment: unexpected argument \"amout\""` | A key in `values` that the method does not have |
| `"unknown method \"incremnt\""` | No function with that name in the ABI |
| `"execution reverted: …"` | The contract reverted; the rest is its reason |
| `"execution reverted"` (nothing after it) | The contract reverted without a reason, for example by calling `sha256` |
| `"contract deployment failed: …"` | Every call to a contract whose constructor failed. Deploy it again |
| `"increment: argument \"amount\": number 1.5 is not an exact integer; pass it as a string"` | Integers must be whole; very large ones must be strings |
