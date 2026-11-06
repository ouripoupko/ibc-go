# API reference

The complete HTTP interface of an ibc node. For a guided introduction with examples, see the
[usage guide](usage.md).

- [Conventions](#conventions)
- [Signatures](#signatures)
- [Values](#values)
- [The node](#the-node): `node_info`
- [Agents](#agents): `register_agent`, `is_exist_agent`
- [Contracts](#contracts): `deploy_contract`, `get_contracts`, `get_contract`
- [Calling a contract](#calling-a-contract): `contract_write`, `contract_read`
- [Membership](#membership): `join_contract`, `leave_contract`, `rotate_key`
- [The event stream](#the-event-stream): `/stream`
- [Node to node](#node-to-node): `a2a_consent`
- [Objects](#objects): event, contract summary, record
- [Limits](#limits)

## Conventions

### Requests

Every call except the event stream goes to one URL family:

```
<VERB> /ibc/app/<agent>/<contract>/<method>?action=<action>
Content-Type: application/json

<JSON body>
```

- `<agent>` is an agent id: the address of the agent's key, such as
  `0x86D637BcBc56F0Dba2b0B5398B4446b5e5330851`, in any letter case. Signed transactions must name the
  agent that signed.
- `<contract>` and `<method>` are given only by the calls that need them. Leave out trailing parts
  you do not need, as in `/ibc/app/0x86D6…?action=register_agent`.
- The verb and the action together select the call. The same action with another verb is
  unknown.
- A body is read only if `Content-Type` is `application/json`.

### Replies

The reply is always JSON. The HTTP status is `200` for every call that the node could parse, even
when the call failed; check the reply itself.

- **Errors** in the request (a bad signature, an unknown contract, an unknown action) come back as
  an object with a `message`:

  ```json
  {"message": "consensus: unknown contract 0x67bb…"}
  ```

- **Contract errors** (a revert, a wrong argument) come back as a string in place of the result:
  `"execution reverted: amount must be positive"`.
- Status `400` means the body is not valid JSON, and `404` means the path has more than three parts
  after `/ibc/app/`.

### Asynchronous calls

`register_agent`, `deploy_contract`, `contract_write`, `join_contract`, `leave_contract` and
`rotate_key` reply at once with a **request id**: the id of the signed transaction (or
registration), a `0x`-prefixed hex hash. Their outcome arrives later as an
[event](#the-event-stream) whose `request` field holds that id. Open the stream before you send
the request.

### CORS

Every reply carries `Access-Control-Allow-Origin: *`. `OPTIONS` requests (browser preflight) are
answered with status `204`, allowing the methods `GET`, `POST`, `PUT` and `DELETE` and the
`Content-Type` header.

### Who may do what

Every change is signed by the agent that makes it, and checked by the node and by every partner.
Every read is signed too, as a [query](#queries):

| Call | Who may make it |
|---|---|
| `contract_read` | Anyone with a key. The contract sees the signer as `msg.sender`, and decides what to answer |
| `get_contracts`, `get_contract`, the event stream | The agent itself only |
| `node_info`, `is_exist_agent` | Anyone, unsigned |
| Node to node (`a2a_consent`) | Nodes, signed with their node key; see [Node to node](#node-to-node) |

The node never shows one agent's data to another, except what a contract's own functions return.
The node's operator can still see everything stored on the node.

## Signatures

Agents sign with their Ethereum key, using [EIP-712](https://eips.ethereum.org/EIPS/eip-712)
typed data: a browser wallet's `eth_signTypedData_v4` (MetaMask, Rabby, …), or `signTypedData` of
ethers.js. The signature is
the usual 65-byte `0x`-hex string.

```js
const domain = { name: 'ibc', version: '1' };
const types = {
  Transaction: [
    { name: 'contract', type: 'string' },   // the contract id; "" for a deploy
    { name: 'kind',     type: 'string' },   // deploy | call | join | leave | rotate_key
    { name: 'agent',    type: 'address' },  // the signer
    { name: 'nonce',    type: 'string' },   // random, to make each transaction unique
    { name: 'body',     type: 'string' },   // JSON text, by kind (below)
  ],
  Registration: [
    { name: 'agent',    type: 'address' },  // the signer
    { name: 'node',     type: 'string' },   // the node's URL
    { name: 'nodeKey',  type: 'address' },  // the node's key, from node_info
    { name: 'nonce',    type: 'string' },
  ],
  Query: [
    { name: 'agent',    type: 'address' },  // the signer: who asks
    { name: 'action',   type: 'string' },   // contract_read | get_contract | get_contracts | stream
    { name: 'target',   type: 'string' },   // the agent whose copy is asked: the URL's <agent>
    { name: 'contract', type: 'string' },   // the URL's <contract>, or ""
    { name: 'body',     type: 'string' },   // JSON text, by action (below), or ""
    { name: 'time',     type: 'uint64' },   // unix seconds when signed
    { name: 'nonce',    type: 'string' },
  ],
};
```

A signed transaction is sent as:

```json
{
  "tx": {"contract": "0x67bb…", "kind": "call", "agent": "0x86D6…", "nonce": "9f2c…",
         "body": "{\"method\":\"increment\",\"values\":{\"amount\":5}}"},
  "signature": "0x…"
}
```

The **transaction id** is the EIP-712 digest of the transaction. A deploy's id is the new contract's
id. The bodies, by kind:

| Kind | Body |
|---|---|
| `deploy` | `{"name", "contract", "protocol", "default_app", "profile", "code", "abi", "constructor", "node", "nodeKey"}`: see [deploy_contract](#deploy_contract) |
| `call` | `{"method": "increment", "values": {"amount": 5}}` |
| `join` | `{"node": "<your node's URL>", "nodeKey": "<your node's key>", "profile": …}` |
| `leave` | `{"member": "<agent id>"}`: yourself to leave, or another member to remove |
| `rotate_key` | `{"node": "<your new node's URL>", "nodeKey": "<its key>"}` |

### Queries

A query is a signed request to see something. It is never ordered or stored, and its EIP-712 type
is not a transaction's, so a signed query can never be used as a transaction. A signed query is:

```json
{
  "query": {"agent": "0x5B38…", "action": "contract_read", "target": "0x86D6…", "contract": "0x67bb…",
            "body": "{\"method\":\"count\",\"values\":{}}", "time": 1789000000, "nonce": "4e1a…"},
  "signature": "0x…"
}
```

- `contract_read` sends it as the request body. The other calls send its JSON in the `auth` URL
  parameter, URL-encoded, because a `GET` (and a browser's `EventSource`) has no body.
- `action`, `target` and `contract` must be those of the call it is sent with.
- `time` must be within 60 seconds of the node's clock, and the node accepts each query only once.
  Sign a new query for every call, including when you reconnect the event stream.
- Errors: `this call needs a signed query`; `query: signature does not match the agent`;
  `query for get_contract sent to contract_read` (and the same for agent and contract); `query
  expired, or its time is wrong: check the clock`; `query already used`; `only agent 0x… may ask
  this`.

| Action | Body |
|---|---|
| `contract_read` | `{"method": "count", "values": {…}}` |
| `get_contracts`, `get_contract`, `stream` | `""` |

### Read sessions

A query can be signed by a **read session** instead of the agent's own key, so that an app can read
without asking the wallet each time. The agent signs a `Session` once, in its wallet; the app then
signs each query with the session's key.

- **Read only.** A session signs queries: `contract_read`, `get_contract`, `get_contracts` and the
  event stream. It can never sign a transaction, a registration or anything else that enters a
  ledger: those take only the agent's own signature. Its `scope` must be `"read"`.
- **One node.** `nodeKey` is the node's key, from `node_info`; other nodes refuse the session.
- **Until a time.** `expires` is in unix seconds. The node refuses a session that has expired, or
  that lasts more than 7 days from now. It keeps no record of sessions: a session cannot be ended
  early on the node, only forgotten by the app, so keep sessions short.
- **The key** is a P-256 (secp256r1) key, the kind browsers make with WebCrypto, which can keep it
  unextractable. `key` is its public key, uncompressed (65 bytes, starting with `04`).

```js
Session: [
  { name: 'agent',    type: 'address' },  // the signer: the agent
  { name: 'nodeKey',  type: 'address' },  // the node it is for, from node_info
  { name: 'key',      type: 'bytes' },    // the session's P-256 public key, uncompressed
  { name: 'scope',    type: 'string' },   // "read"
  { name: 'expires',  type: 'uint64' },   // unix seconds
  { name: 'nonce',    type: 'string' },
],
```

A query signed by a session carries the signed session with it, and its `signature` is the session
key's:

```json
{
  "query": {"agent": "0x86d6…", "action": "contract_read", …},
  "signature": "0x…",
  "session": {
    "session": {"agent": "0x86d6…", "nodeKey": "0x57E3…", "key": "0x04…", "scope": "read",
                "expires": 1789030000, "nonce": "…"},
    "signature": "0x…"
  }
}
```

- The session key signs, with ECDSA P-256 and SHA-256, the query's fields one per line:
  `"ibc query v1"`, `agent` (lower-case hex), `action`, `target`, `contract`, `body`, `time`
  (decimal) and `nonce`, joined with `\n`. The signature is 64 bytes, `r` then `s`, as WebCrypto
  gives it.
- The query's `agent` must be the session's `agent`. The rest of the query's rules are unchanged:
  60 seconds, accepted once, bound to its action, agent and contract.
- Errors: `session for node key 0x…, this node's key is 0x…`; `session expired`; `session lasts
  longer than 168h0m0s`; `session scope "…": only "read" is allowed`; `session: signature does not
  match the agent`; `query: signature does not match the session key`.

## Values

Contract arguments are sent in `values`, an object keyed by the parameter names in the contract's
ABI. Every parameter must be given, and no others. A parameter without a name is keyed by its
position: `"0"`, `"1"`, and so on.

| ABI type | Sent as | Returned as |
|---|---|---|
| `uint<N>`, `int<N>` | A JSON number, or a string: decimal (`"12"`) or hex (`"0x0c"`). A number must be a whole number of at most 2^53; send anything larger as a string | A JSON number, exact |
| `bool` | `true` / `false` | `true` / `false` |
| `string` | A JSON string | A JSON string |
| `address` | A hex string, `"0x"` and 40 digits | A checksummed hex string |
| `bytes`, `bytes<N>` | A hex string, `"0x…"`. `bytes<N>` needs exactly N bytes | A hex string, `"0x…"` |
| `T[]`, `T[N]` | A JSON array. `T[N]` needs exactly N elements | A JSON array |
| tuple / struct | An object keyed by the component names | An object keyed by the component names |

A function's result is `null` if it returns nothing, the value itself if it returns one value, and
an array if it returns several.

> **JavaScript.** `JSON.parse` keeps integers exact only up to 2^53. Results larger than that are
> correct in the reply, but lose precision when parsed as JavaScript numbers.

## The node

### `node_info`

```
GET /ibc/app?action=node_info
```

**Reply:** `{"nodeKey": "0x57E3…"}`, the address of the node's key. Registrations, deploys and joins
name it.

## Agents

### `register_agent`

Registers an agent on the node. The agent authorizes the node's key to sign consensus blocks for
it. Registering again updates the registration.

```
PUT /ibc/app/<agent>?action=register_agent
{
  "registration": {"agent": "0x86D6…", "node": "http://localhost:2611",
                   "nodeKey": "0x57E3…", "nonce": "…"},
  "signature": "0x…"
}
```

| Field | Meaning |
|---|---|
| `agent` | The agent; must sign, and must be the agent in the URL |
| `node` | The node's URL, at which partners' nodes reach this agent |
| `nodeKey` | This node's key, from `node_info` |

**Reply:** a request id. **Event:** `register_agent` with `reply: true` and `contract: null`.

**Errors:** `signature does not match the agent`; `registration of 0x… sent as 0x…`;
`registration authorizes node key 0x…, this node's key is 0x…`.

### `is_exist_agent`

```
GET /ibc/app/<agent>?action=is_exist_agent
```

**Reply:** `true` if the agent is registered on this node, otherwise `false`.

## Contracts

### `deploy_contract`

Deploys a contract. The deploying agent is its first partner, and the contract's constructor runs
with the agent's address as `msg.sender`.

```
PUT /ibc/app/<agent>?action=deploy_contract
{"tx": {"contract": "", "kind": "deploy", "agent": "0x86D6…", "nonce": "…", "body": "<JSON>"}, "signature": "0x…"}
```

The body:

| Field | Required | Meaning |
|---|---|---|
| `code` | yes | The contract's bytecode as compiled (`.bin`), as hex, with or without `0x` |
| `abi` | yes | The contract's ABI (`.abi`), as a JSON array |
| `constructor` | if the constructor has parameters | The constructor's arguments, as [values](#values) |
| `node` | yes | This node's URL |
| `nodeKey` | yes | This node's key, from `node_info` |
| `protocol` | no | The consensus protocol: `cordial` (the default; `BFT` means the same) or `immediate`, which orders the creator's own transactions locally, with no partners, for testing |
| `name`, `contract`, `default_app`, `profile` | no | Labels kept with the contract and returned by `get_contract` |

**Reply:** the transaction id, which is the new contract's id. Use it as `<contract>` in every later
call.

**Event:** `deploy_contract`, with `contract` set to the contract's id. `reply` is `true` if the
contract was deployed, or the error as a string, for example
`"execution reverted: a poll needs at least two options"` or
`"constructor: missing argument \"start\""`. A contract whose deployment failed stays failed: every
later call to it is answered with `"contract deployment failed: <reason>"`.

**Errors:** `unknown agent "0x…"` (register first); `deploy names node key 0x…, this node's key is
0x…`; `consensus: unknown protocol "…"`.

### `get_contracts`

```
GET /ibc/app/<agent>?action=get_contracts&auth=<signed query>
```

Only the agent itself may ask: the [query](#queries) is signed by `<agent>`.

**Reply:** an array of [contract summaries](#contract-summary), one for each contract the agent
has on this node, sorted by contract id.

### `get_contract`

```
GET /ibc/app/<agent>/<contract>?action=get_contract&auth=<signed query>
```

Only the agent itself may ask: the [query](#queries) is signed by `<agent>`, for `<contract>`.

**Reply:** the [contract summary](#contract-summary), or `{}` if the agent has no such contract.

## Calling a contract

### `contract_write`

A transaction: runs a contract function and keeps its changes. All partners apply it, in the order
the consensus protocol gives.

```
POST /ibc/app/<agent>/<contract>/<method>?action=contract_write
{"tx": {"contract": "<contract>", "kind": "call", "agent": "<agent>", "nonce": "…",
        "body": "{\"method\":\"<method>\",\"values\":{…}}"}, "signature": "0x…"}
```

The contract and method in the URL must match the transaction's.

**Reply:** the transaction id.

**Event:** `contract_write`, with `reply` set to the function's result, or the error as a string. If
the function fails, nothing it did is kept, and `state_root` is unchanged.

| Error reply | Meaning |
|---|---|
| `"execution reverted: <reason>"` | The function reverted with this reason (`require`, `revert`, or a custom error, shown as `Name(arg, …)`) |
| `"execution reverted"` | The function reverted without a reason |
| `"unknown method \"<method>\""` | The ABI has no function with this name |
| `"<method>: missing argument \"<name>\""` | A parameter is missing from `values` |
| `"<method>: unexpected argument \"<name>\""` | `values` has a key the function does not have |
| `"<method>: argument \"<name>\": <problem>"` | A value of the wrong type or out of range |
| `"out of gas"` | The call used more than 100 million gas |
| `"contract deployment failed: <reason>"` | The contract's constructor failed |
| `"get_partners: not implemented"` | Also `get_parameters`: built-in methods that are not defined yet |

**Errors** (request not accepted): `consensus: unknown contract 0x…` (the agent is not a partner
of this contract on this node); `transaction signed by 0x…, sent as 0x…`; `signature does not
match the agent`; `0x… is not a member`.

### `contract_read`

Runs a contract function without keeping any change, like `eth_call`. Nothing is added to the
ledger, and no event is sent. The body is a signed [query](#queries):

```
POST /ibc/app/<agent>/<contract>/<method>?action=contract_read
{"query": {"agent": "<signer>", "action": "contract_read", "target": "<agent>", "contract": "<contract>",
           "body": "{\"method\":\"getValue\",\"values\":{\"who\":\"0x86D6…\"}}",
           "time": 1789000000, "nonce": "…"},
 "signature": "0x…"}
```

`<agent>` is whose copy of the contract to read, on this node. Anyone may sign the query: the
function sees `msg.sender` set to **the signer**, so the contract decides what each reader may see,
for example with `require(isMember[msg.sender])`. The `<method>` in the URL is optional; if given,
it must match the query's. The function sees the latest state, and `block.timestamp` set to the
time of the request.

**Reply:** the function's result, or the error as a string (the same strings as for
`contract_write`). `{"message": "no such contract"}` if the agent has no such contract, or its
deployment has not finished yet.

## Membership

A contract's partners change through `join`, `leave` and `rotate_key` transactions, which are
ordered like every other transaction. While one is being decided, the contract holds back other
transactions (see [consensus design](consensus.md#10-membership-changes)).

### `join_contract`

Asks a partner of a contract to let the agent join. Send it to **your own** node, which passes the
signed join to the partner's node.

```
PUT /ibc/app/<agent>?action=join_contract
{
  "tx": {"contract": "<contract>", "kind": "join", "agent": "<agent>", "nonce": "…",
         "body": "{\"node\":\"<your node's URL>\",\"nodeKey\":\"<your node's key>\",\"profile\":\"\"}"},
  "signature": "0x…",
  "partner": {"address": "<the partner's node URL>", "agent": "<the partner's agent id>"}
}
```

**Reply:** the transaction id, or `null` if the agent already has the contract on this node.

**Event:** `join_contract` on the joining agent's stream, with `request` set to the transaction id.
`reply` is `true` once this node holds the contract and takes part in it, or `false` if the
contract refused. The decision is the contract's `approve_partner(address)` function, called as the
joining agent; a contract without it accepts everyone. The partners' own streams see a `join`
event with the decision.

**Errors:** `join_contract needs partner.address and partner.agent`; the partner's node's errors,
for example `consensus: unknown contract 0x…`.

### `leave_contract`

Leaves a contract, or asks to remove another member.

```
PUT /ibc/app/<agent>?action=leave_contract
{"tx": {"contract": "<contract>", "kind": "leave", "agent": "<agent>", "nonce": "…",
        "body": "{\"member\":\"<agent id>\"}"}, "signature": "0x…"}
```

**Reply:** the transaction id. **Event:** `leave`, with `reply` set to the decision. Leaving
yourself is always approved. Removing another member is decided by the contract's
`approve_removal(address)` function, called as you; a contract without it refuses.

### `rotate_key`

Moves an agent's part in a contract to another node, or replaces a compromised node key. Send it to
your current node.

```
PUT /ibc/app/<agent>?action=rotate_key
{"tx": {"contract": "<contract>", "kind": "rotate_key", "agent": "<agent>", "nonce": "…",
        "body": "{\"node\":\"<new node URL>\",\"nodeKey\":\"<new node key>\"}"}, "signature": "0x…"}
```

**Reply:** the transaction id. **Event:** `rotate_key` with `reply: true`. The new node receives the
contract's history from the partners and takes over; the old node stops taking part. Register the
agent on the new node first.

## The event stream

```
GET /stream?auth=<signed query>[&auth=<signed query>…]
```

A [Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events)
stream of the outcomes of asynchronous calls.

- Each `auth` is a [query](#queries) with action `stream`, signed by the agent whose events it
  subscribes to, with `target` set to that agent. Only the agent itself may open its stream.
- The query's `contract` chooses the events: empty for every event of the agent, or a contract
  id for that contract's events only.
- Repeat `auth` to subscribe to several agents or contracts. An event is delivered once if it
  matches any of them.
- A missing, expired or wrong query is refused with status `401` and a `{"message": …}` body.

The stream starts with an empty message (`data: `), which tells you the subscription is active.
After that every event is one message, and an empty message is sent every 10 seconds to keep the
connection open:

```
data: 

data: {"request":"0x9a00…","contract":"0x67bb…","action":"contract_write","reply":15,"state_root":"0x…","agent":"0x86D6…"}

data: 
```

In a browser, `EventSource` handles the stream:

```js
const auth = await ibc.query('stream');   // a signed query; see web/static/ibc.mjs
const events = new EventSource(`http://localhost:2611/stream?auth=${encodeURIComponent(JSON.stringify(auth))}`);
events.onmessage = (e) => {
  if (!e.data) return;                 // keep-alive
  const event = JSON.parse(e.data);    // see "Event" below
};
```

If a client falls more than 256 events behind, the node closes its stream, and the events in
between are lost. `EventSource` reconnects by itself with the same URL, which the node refuses,
since each query is accepted once: on an error, close it and open a new stream with a new query. When the node shuts down, it closes
every stream.

## Node to node

These calls are sent by nodes to each other, not by apps. They are listed so that a node can be
implemented or debugged against them.

### `a2a_consent`

The consensus protocol's messages, addressed to an agent's miner of a contract. Every message is
signed by the sending node's key:

```
PUT /ibc/app/<agent>/<contract>?action=a2a_consent
{"msg": {"type": "<type>", …}, "node": "<sending node's key>", "time": 1789000000, "signature": "0x…"}
```

- The signature is over `keccak256("ibc a2a v1\n" + agent + "\n" + contract + "\n" + time + "\n" + msg)`,
  where `agent` is the checksummed address in the URL, `time` is decimal, and `msg` is the exact
  JSON text of `msg` in the body. It is a 65-byte signature like an agent's, by `node`.
- `time` is when the message was sent, and must be within 5 minutes of the receiving node's clock.
  A node signs when it sends, so a message it retries carries a new time.
- `blocks`, `tx` and `join_result` are accepted from any node: blocks and transactions carry their
  own signatures. `get_blocks`, `get_from` and `get_ledger` are answered only for a node whose key
  belongs to a member of the contract's current configuration. A `welcome` must be signed by the
  welcoming member's node key, as the configuration in it records it.

| `type` | Fields | Reply |
|---|---|---|
| `blocks` | `blocks`: hex RLP encodings of signed blocks | `null` |
| `get_blocks` | `hashes`: block hashes | `{"blocks": [...]}` |
| `get_from` | `depth`: a round | `{"blocks": [...]}`: the blocks from that round on |
| `get_ledger` | none | `{"records": [...]}`: every [record](#record) of the contract, in order. A newcomer's node replays them to build the contract's state |
| `tx` | `tx`: a signed transaction to put in the miner's mempool (a relayed join, for example) | the transaction id |
| `welcome` | `config`: the contract's new configuration; `sender`: the welcoming member | `null`; the node then fetches the ledger from the sender (`get_ledger`) and joins |
| `join_result` | `txId`, `approved: false`: a refused join | `null` |

## Objects

### Event

| Field | Meaning |
|---|---|
| `request` | The id of the request this event answers |
| `contract` | The contract's id, or `null` for `register_agent` |
| `action` | `register_agent`, `deploy_contract`, `contract_write`, `join` (a member's view of a join), `leave`, `rotate_key`, or `join_contract` (the joining agent's view) |
| `reply` | The outcome: the function's result for `contract_write`, `true`/`false` for the others, or an error string |
| `state_root` | The contract's state root after the transaction, as a hex hash. All zeros if the contract was never deployed |
| `agent` | The subscribed agent the event belongs to |

The state root summarizes the contract's entire state. Partners that processed the same
transactions have the same state root.

### Contract summary

Returned by `get_contracts` and `get_contract`. Fields are present only when they have a value.

| Field | Meaning |
|---|---|
| `id` | The contract's id |
| `name`, `contract`, `protocol`, `default_app`, `profile` | The labels given at deployment |
| `pid` | The creating agent |
| `address` | The creating agent's node URL |
| `code`, `abi` | The bytecode and ABI given at deployment |
| `evm_address` | The contract's address in its own EVM state |
| `state_root` | The state root after the last successful transaction |
| `block_number` | How many transactions the contract has processed |
| `deploy_error` | Why the deployment failed, if it did |

### Record

A transaction as the ledger keeps it.

| Field | Meaning |
|---|---|
| `type` | `TX` |
| `action` | `deploy_contract`, `contract_write`, `join`, `leave` or `rotate_key` |
| `agent` | The signing agent |
| `contract` | The contract's id |
| `method` | For `contract_write`, the function |
| `message` | The transaction's body, decoded |
| `timestamp` | The time of the consensus block that carried it: `YYYYMMDDHHMMSSffffff`, UTC |
| `hash_code` | The transaction id |
| `index` | Its position in the contract's ledger |
| `tx` | The signed transaction itself, so anyone can check the agent's signature |

## Limits

| What | Limit |
|---|---|
| Request body | 16 MiB |
| Gas per call | 100,000,000 |
| Events a stream client may fall behind | 256 |
| Event stream keep-alive | every 10 seconds |
| Requests to other nodes | 30 seconds |
