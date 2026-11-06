# Usage guide

This guide takes you from a running node to a contract you share with other agents. It assumes a
node is running at `http://localhost:2611`; the [README](../README.md) shows how to start one.
Every call used here is described in full in the [API reference](api.md).

- [Concepts](#concepts)
- [Your key](#your-key)
- [The web interface](#the-web-interface)
- [Write a contract](#write-a-contract)
- [Compile it](#compile-it)
- [Call ibc from the shell](#call-ibc-from-the-shell)
- [Call ibc from JavaScript](#call-ibc-from-javascript)
- [The example contracts](#the-example-contracts)
- [Sharing a contract](#sharing-a-contract)
- [Without the helpers: curl](#without-the-helpers-curl)
- [Common errors](#common-errors)

## Concepts

- **Agent.** You, as ibc sees you. Your agent id is the address of your Ethereum key, for example
  `0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851`. You register it once on the node you choose.
- **Node.** The server your agent lives on. It runs your contracts and talks to your partners'
  nodes, but it never holds your key: everything you do is signed by you.
- **Contract.** A Solidity program you deploy. Deploying returns the contract's id, which every
  later call uses. Each contract has its own state.
- **Partners.** The agents who share a contract. Each partner's node keeps its own copy, and the
  partners agree on the order of every transaction with the [consensus protocol](consensus.md).
- **Writes and reads.** A *write* (`contract_write`) is a signed transaction: it can change the
  contract's state, all partners apply it, and its result arrives on the event stream. A *read*
  (`contract_read`) changes nothing and answers at once. Reads are signed too: the contract sees
  the reader as `msg.sender`, and can refuse readers it does not want.
- **Event stream.** A long-lived HTTP connection (`/stream`) on which the node reports the outcome
  of every deploy, write, join and leave. Open it before you send a write.

## Your key

Every transaction is signed with your Ethereum key, using EIP-712 (a browser wallet's
`eth_signTypedData_v4`). The node checks each signature, and so does every partner.

- **In a web app**, the user signs in a **browser wallet**, such as MetaMask, Rabby or Coinbase
  Wallet. The JavaScript client below does that for you.
- **From the shell**, keep a key in a file and let the `ibc` program sign with it:

  ```sh
  ./ibc key new ~/.ibc/alice.key        # prints the new key's address: your agent id
  ./ibc key address ~/.ibc/alice.key    # prints it again
  ```

  The file holds a private key. Keep it private (it is created readable by you only), and keep a
  backup: losing it means losing your agent.

When you register, you also authorize your node's own key to sign *consensus blocks* for you. That
key cannot sign transactions in your name; it only takes part in ordering them.

## The web interface

The node serves a web interface at its own address, for example `http://localhost:2611`. Open it in
a browser that has a wallet: it holds your key, and the page asks it to sign every call. The node
never sees your key.

Any browser wallet for Ethereum keys works: [MetaMask](https://metamask.io),
[Rabby](https://rabby.io), [Coinbase Wallet](https://www.coinbase.com/wallet),
[Trust Wallet](https://trustwallet.com), [Frame](https://frame.sh), or the wallet built into
[Brave](https://brave.com/wallet/). Your agent is your key, not the wallet app, so the same key in
another wallet is the same agent. Smart-contract wallets, such as Safe, do not work: ibc identifies
you by a key's own signature. Mobile wallets that connect by QR code (WalletConnect) are not
supported yet.

- **Your contracts.** Connect your wallet, and register on the node the first time. The page lists
  your contracts on this node; it cannot list other agents' contracts.
- **Deploy.** Choose the compiled contract's `.bin` and `.abi` files ([Compile it](#compile-it)
  shows how to make them), or paste them. The page shows a field for each constructor argument.
- **Join.** Paste an invite from a member, or enter the contract id, the member's node URL and the
  member's agent id. The contract's own rules decide.
- **A contract's page.** Click a contract to see its details and a form for each function in its
  ABI. *Read* runs a `view` function and shows the result at once; *Send* sends a transaction and
  shows its result once all members have applied it. Reverts are shown in red. The page also
  shows an **invite** to give to people who should join, and a button to leave.

The bottom of each page shows the node's storage: the size of its data folder.

Arguments are typed as in [Arguments](#arguments): numbers and addresses as plain text, arrays and
structs as JSON.

**Each signature is named.** While the wallet waits, the page shows a box such as *Sign request:
increment 7*. It closes when the wallet answers.

**Your wallet asks for each transaction:** each deploy, write, join and leave, and the registration.
**Reading does not ask each time.** The first time the page needs to read (to list your contracts,
open a contract, read, or open the event stream), your wallet asks you to approve a *reading
session*: a key the browser makes, which can only read, only on this node, for 8 hours. The page
signs every read with it. The header shows until when it lasts; **End** forgets it, and the next
read asks again.

- The session key cannot sign a transaction: the node accepts transactions only with your own
  signature. A session can only let someone read what you can read, on this node, until it ends.
- The browser keeps the key unextractable, in this site's storage, so moving between pages or
  reloading keeps the session. Closing the browser does not end it; **End** does, or the time does.
- Sessions need a secure page: `https`, or `http://localhost`. Elsewhere the page asks the wallet
  for every read.

Wallets work with pages at `http://localhost`. To use a node over the network, put it behind HTTPS,
as with any page that uses a wallet.

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
| `msg.sender`, `tx.origin` | The agent who signed the transaction: its key's address |
| `block.timestamp` | The time of the consensus block that carried the transaction, in seconds |
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

Every partner runs the same transactions in the same order and must reach the same state, so a
contract cannot see anything that differs between nodes. That is why the fields above are fixed.

Solidity features up to the Osaka fork work, so recent compilers can be used with their default
settings.

### Who may join, and who may remove

A contract can decide its own membership by defining these functions. ibc calls them when a join or
a removal is ordered:

```solidity
/// Called as the joining agent. Return true to let it join.
function approve_partner(address partner) external view returns (bool);

/// Called as the member who asks to remove another. Return true to remove it.
function approve_removal(address member) external view returns (bool);
```

- **Without `approve_partner`**, every join is approved.
- **Without `approve_removal`**, members cannot remove each other. Any member can always leave on
  its own.

[`testdata/Club.sol`](../testdata/Club.sol) is an example: the owner invites agents with
`invite(address)`, only invited agents may join, and only the owner may remove others.

### Tips

- **Return what the client needs.** A write's return value is the only result the client gets
  back from it, so return the new id or the new value.
- **Use `view` functions for reads.** With `contract_read`, nothing changes anyway: the call runs
  on a copy of the state, which is then thrown away.
- **Use `msg.sender` for "me".** A function like `myVote()` or `whoAmI()` lets an agent ask the
  contract about itself.
- **Decide who may read.** A read's `msg.sender` is whoever signed it, and anyone can sign one. To
  keep something private, check the reader in the `view` function, for example
  `require(isMember[msg.sender], "members only")`. A function without a check is public.
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

[`examples/scripts/ibc.sh`](../examples/scripts/ibc.sh) is a small client built on `curl`, `jq`
and the `ibc` program, which signs with your key file. It waits for each write's result on the
event stream and prints just the result.

```sh
export IBC_URL=http://localhost:2611   # the node (this is the default)
export IBC_KEY=~/.ibc/alice.key        # your key file
export IBC_BIN=./ibc                   # the ibc program
alias ibc=examples/scripts/ibc.sh

ibc newkey                             # once: creates IBC_KEY
ibc register
# registered 0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851 at http://localhost:2611

C=$(ibc deploy Counter '{"start": 10}')       # deploys build/Counter.bin, prints the contract id

ibc write "$C" increment '{"amount": 5}'
# 15
ibc write "$C" increment '{"amount": 0}'
# "execution reverted: amount must be positive"

ibc read "$C" count
# 15
ibc read "$C" myContribution
# 5
```

Other commands:

```sh
ibc whoami                 # your agent id
ibc exists                 # are you registered on the node?
ibc contracts              # your contracts
ibc contract "$C"          # one contract's details
ibc stream                 # follow your events (Ctrl-C to stop)
```

Run `examples/scripts/ibc.sh` without arguments for the full list.

## Call ibc from JavaScript

[`web/static/ibc.mjs`](../web/static/ibc.mjs) is a small client without dependencies; the web
interface uses it, and every node serves it at `/ibc.mjs`. It works
in browsers and in Node.js 18 and later. It signs through a *signer*: a browser wallet, or
any object with `getAddress()` and `signTypedData(domain, types, message)`, such as an ethers.js
wallet. It opens the event stream for you and resolves each write with its result:

```js
import { Ibc } from './ibc.mjs';

const ibc = await Ibc.connect('http://localhost:2611', Ibc.wallet(window.ethereum));
await ibc.register();                    // the wallet asks the user to sign

const code = await (await fetch('/build/Counter.bin')).text();
const abi = await (await fetch('/build/Counter.abi')).text();
const counter = await ibc.deploy({ name: 'my-counter', code, abi, constructor: { start: 10 } });

await ibc.write(counter, 'increment', { amount: 5 });   // 15 (one signature)
await ibc.read(counter, 'count');                       // 15 (one signature)

try {
  await ibc.write(counter, 'increment', { amount: 0 });
} catch (err) {
  console.log(err.message);   // execution reverted: amount must be positive
}

ibc.close();
```

Every write, deploy, join and leave asks the user to approve a signature in the wallet. So does
each read and opening the event stream, unless the client has a **read session**: set
`ibc.sessionHours = 8` (for example), and the first read asks the wallet once to approve a session
key, which then signs every read without asking. A session can only read, only on this node, and
never signs a transaction. `ibc.startSession(hours)` starts one at once, `ibc.endSession()` forgets
it, and `ibc.useSession(kept)` takes one kept from before (the web interface keeps it in IndexedDB).
See [read sessions](api.md#read-sessions).

[`examples/js/demo.mjs`](../examples/js/demo.mjs) deploys and calls all three examples from
Node.js, signing with an ethers.js wallet:

```sh
cd examples/js && npm install && node demo.mjs http://localhost:2611
```

> **Large numbers in JavaScript.** Results are exact JSON numbers. `JSON.parse` keeps integers
> exactly only up to 2^53. For values that can grow beyond that, return them as `string` from the
> contract, or parse the reply with a big-number-aware parser. When you *send* a large integer,
> pass it as a string: `{"amount": "1000000000000000000000"}`.

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
# {"author":"0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851","text":"hello ibc","time":1793900673}
ibc read "$B" whoAmI
# "0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851"
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
| `address` | `"0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851"` | the same, checksummed |
| `bytes`, `bytes32` | `"0x00ff…"` | `"0x00ff…"` |
| `string[]`, `uint256[3]` | `["a", "b"]` | `["a", "b"]` |
| struct | `{"author": "0x…", "text": "hi"}` | the same |

A parameter without a name, such as the key of a `public` mapping's getter, is passed by its
position: `ibc read "$C" contributions '{"0": "0x86D6…"}'`.

## Sharing a contract

Two people, each with an agent on their own node: alice on node 1, bob on node 2.

```sh
# alice, on node 1
export IBC_URL=http://node1:2611 IBC_KEY=~/.ibc/alice.key
C=$(ibc deploy Counter '{"start": 10}')
ibc whoami                       # alice tells bob the contract id and her agent id

# bob, on node 2
export IBC_URL=http://node2:2611 IBC_KEY=~/.ibc/bob.key
ibc join "$C" http://node1:2611 0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851
# joined 0x67bb…
ibc write "$C" increment '{"amount": 7}'
# 22

# alice sees bob's write
ibc read "$C" count
# 22
```

What happens:

1. Bob's join is a transaction signed by bob. His node passes it to alice's node, and the contract's
   partners order it like any other transaction.
2. When it is ordered, the contract decides with its `approve_partner` function (Counter has none,
   so the join is approved).
3. Bob's node receives the contract's history from alice's node, replays it, checks that it reaches
   the same state, and from then on takes part in consensus. Bob's stream reports `join_contract`
   with `true`; a refused join reports `false`.

While a membership change is being decided, the contract pauses other transactions; they are
processed right after. To leave:

```sh
ibc leave "$C"                    # bob leaves
ibc leave "$C" 0x…                # ask to remove another member (needs approve_removal)
```

Keep in mind:

- **Partners must be online.** A contract makes progress only while a supermajority of its
  partners' nodes are reachable; with two partners, both. See the
  [consensus design](consensus.md#1-goals).
- **Partners' nodes must reach each other** at the addresses the agents registered.

## Without the helpers: curl

Every call is one HTTP request. A write carries a signed transaction, and a read or the event
stream a signed query. The `ibc` program signs; `jq` builds the JSON. Open the event stream in one
terminal (the signed query goes in the URL):

```sh
curl -sN -G http://localhost:2611/stream \
  --data-urlencode "auth=$(./ibc sign query -key ~/.ibc/alice.key -action stream)"
```

and send the requests from another:

```sh
IBC=http://localhost:2611
KEY=~/.ibc/alice.key
ME=$(./ibc key address $KEY)
NODE_KEY=$(curl -s "$IBC/ibc/app?action=node_info" | jq -r .nodeKey)

# Register: a registration signed with your key.
./ibc sign registration -key $KEY -node $IBC -node-key $NODE_KEY |
  curl -s -X PUT "$IBC/ibc/app/$ME?action=register_agent" -H 'Content-Type: application/json' -d @-

# Deploy: a deploy transaction. The reply is the new contract's id.
BODY=$(jq -cn --arg node $IBC --arg nodeKey $NODE_KEY \
  --arg code "$(cat examples/build/Counter.bin)" --slurpfile abi examples/build/Counter.abi \
  '{name: "my-counter", code: $code, abi: $abi[0], constructor: {start: 10}, node: $node, nodeKey: $nodeKey}')
CONTRACT=$(./ibc sign tx -key $KEY -kind deploy -body "$BODY" |
  curl -s -X PUT "$IBC/ibc/app/$ME?action=deploy_contract" -H 'Content-Type: application/json' -d @- | jq -r .)

# Write: a call transaction. The reply is its id; the result appears on the event stream.
./ibc sign tx -key $KEY -kind call -contract $CONTRACT -body '{"method": "increment", "values": {"amount": 5}}' |
  curl -s -X POST "$IBC/ibc/app/$ME/$CONTRACT/increment?action=contract_write" -H 'Content-Type: application/json' -d @-

# Read: a signed query. The reply is the result.
./ibc sign query -key $KEY -action contract_read -contract $CONTRACT -body '{"method": "count", "values": {}}' |
  curl -s -X POST "$IBC/ibc/app/$ME/$CONTRACT/count?action=contract_read" -H 'Content-Type: application/json' -d @-
```

On the stream, each result is one line:

```
data: {"request":"0x5d2c…","contract":null,"action":"register_agent","reply":true,"agent":"0x86D6…"}
data: {"request":"0x67bb…","contract":"0x67bb…","action":"deploy_contract","reply":true,"state_root":"0x…","agent":"0x86D6…"}
data: {"request":"0x9a00…","contract":"0x67bb…","action":"contract_write","reply":15,"state_root":"0x…","agent":"0x86D6…"}
```

Match `request` with the id you got when you sent the request. Empty `data:` lines are
keep-alives.

## Common errors

| Reply | Meaning |
|---|---|
| `{"message": "… signature does not match the agent"}` | The transaction was changed after it was signed, or signed by another key |
| `{"message": "transaction signed by 0x…, sent as 0x…"}` | The agent in the URL is not the one who signed |
| `{"message": "registration authorizes node key 0x…, this node's key is 0x…"}` | Sign the registration with the `nodeKey` from this node's `node_info` |
| `{"message": "unknown agent \"0x…\""}` | Register first |
| `{"message": "consensus: unknown contract 0x…"}` | You are not a partner of this contract on this node: check the id, or join it first |
| `{"message": "no such contract"}` | A read of a contract you do not have, or whose deployment has not finished |
| `{"message": "this call needs a signed query"}` | Reads, `get_contract(s)` and the event stream are signed: see the [API reference](api.md#queries) |
| `{"message": "query expired, or its time is wrong: check the clock"}` | Sign the query just before you send it; the node accepts it for 60 seconds |
| `{"message": "query already used"}` | Every query is accepted once: sign a new one for each call |
| `"increment: missing argument \"amount\""` | A key in `values` is missing or misspelled. Names must match the ABI exactly |
| `"increment: unexpected argument \"amout\""` | A key in `values` that the method does not have |
| `"unknown method \"incremnt\""` | No function with that name in the ABI |
| `"execution reverted: …"` | The contract reverted; the rest is its reason |
| `"execution reverted"` (nothing after it) | The contract reverted without a reason, for example by calling `sha256` |
| `"contract deployment failed: …"` | Every call to a contract whose constructor failed. Deploy it again |
| `"increment: argument \"amount\": number 1.5 is not an exact integer; pass it as a string"` | Integers must be whole; very large ones must be strings |
