# API reference

The complete HTTP interface of an ibc node. For a guided introduction with examples, see the
[usage guide](usage.md).

- [Conventions](#conventions)
- [Values](#values)
- [Agents](#agents): `register_agent`, `is_exist_agent`
- [Contracts](#contracts): `deploy_contract`, `get_contracts`, `get_contract`
- [Calling a contract](#calling-a-contract): `contract_write`, `contract_read`
- [Joining a contract](#joining-a-contract): `join_contract`
- [The event stream](#the-event-stream): `/stream`
- [Node to node](#node-to-node): `a2a_connect`, `a2a_reply_join`, `a2a_consent`, `a2a_get_ledger`
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

- `<agent>` is the agent the request acts as. For a contract call it is the caller, which becomes
  `msg.sender`.
- `<contract>` and `<method>` are given only by the calls that need them. Leave out trailing parts
  you do not need, as in `/ibc/app/alice?action=register_agent`.
- The verb and the action together select the call. The same action with another verb is
  unknown.
- A body is read only if `Content-Type` is `application/json`. Otherwise the request has no body.

### Replies

The reply is always JSON. The HTTP status is `200` for every call that the node could parse, even
when the call failed; check the reply itself.

- **Errors** in the request (an unknown agent, an unknown contract, an unknown action) come back as
  an object with a `message`:

  ```json
  {"message": "unknown contract \"9f2c…\" of agent \"bob\""}
  ```

- **Contract errors** (a revert, a wrong argument) come back as a string in place of the result:
  `"execution reverted: amount must be positive"`.
- Status `400` means the body is not valid JSON, and `404` means the path has more than three parts
  after `/ibc/app/`.

### Asynchronous calls

`register_agent`, `deploy_contract`, `contract_write` and the node-to-node calls reply at once with
a **request id**, a 64-character hex string. Their outcome arrives later as an
[event](#the-event-stream) whose `request` field holds that id. Open the stream before you send the
request.

The request id is the SHA-256 of the request record, which includes the time the node received it,
so every request has a different id.

### CORS

Every reply carries `Access-Control-Allow-Origin: *`. `OPTIONS` requests (browser preflight) are
answered with status `204`, allowing the methods `GET`, `POST`, `PUT` and `DELETE` and the
`Content-Type` header.

### Authentication

There is none. Anyone who can reach a node can act as any agent on it.

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

A function's result is:

- `null` if it returns nothing,
- the value itself if it returns one value,
- an array if it returns several values (in the order of the function's `returns`).

> **JavaScript.** `JSON.parse` keeps integers exact only up to 2^53. Results larger than that are
> correct in the reply, but lose precision when parsed as JavaScript numbers.

## Agents

### `register_agent`

Registers an agent on the node, with the address at which other nodes reach it. Registering again
updates the address.

```
PUT /ibc/app/<agent>?action=register_agent
{"address": "http://localhost:2611"}
```

| Body field | Meaning |
|---|---|
| `address` | The node URL that partners use to reach this agent |

**Reply:** a request id.

**Event:** `register_agent` with `reply: true` and `contract: null`.

### `is_exist_agent`

```
GET /ibc/app/<agent>?action=is_exist_agent
```

**Reply:** `true` if the agent is registered on this node, otherwise `false`.

## Contracts

### `deploy_contract`

Deploys a contract for the agent. The contract runs its constructor with the agent's address as
`msg.sender`.

```
PUT /ibc/app/<agent>?action=deploy_contract
{
  "name": "my-counter",
  "contract": "Counter.sol",
  "protocol": "BFT",
  "default_app": "https://my-app.example.com",
  "pid": "alice",
  "address": "http://localhost:2611",
  "profile": "",
  "code": "6080604052…",
  "abi": "[{\"inputs\":[{\"name\":\"start\",\"type\":\"uint256\"}],\"type\":\"constructor\"}, …]",
  "constructor": {"start": 10}
}
```

| Body field | Required | Meaning |
|---|---|---|
| `code` | yes | The contract's bytecode as compiled (`.bin`), as hex, with or without `0x` |
| `abi` | yes | The contract's ABI (`.abi`): the JSON text, or the JSON array itself |
| `constructor` | if the constructor has parameters | The constructor's arguments, as [values](#values) |
| `pid` | expected | The creating agent's id: the constructor's `msg.sender`, and the contract's first partner. Use the agent in the URL. Not checked: without it, the constructor runs as the agent `""` |
| `address` | expected | The creating agent's node URL, stored with the first partner. Not checked |
| `name`, `contract`, `protocol`, `default_app`, `profile` | no | Labels kept with the contract and returned by `get_contract`. ibc does not interpret them yet |

Any other fields are kept with the contract as well.

**Reply:** a request id, which is also the new contract's id. Use it as `<contract>` in every
later call.

**Event:** `deploy_contract`, with `contract` set to the contract's id. `reply` is `true` if the
contract was deployed, or the error as a string, for example
`"execution reverted: a poll needs at least two options"` or
`"constructor: missing argument \"start\""`. A contract whose deployment failed stays failed:
every later call to it is answered with `"contract deployment failed: <reason>"`.

**Errors:**

| Message | Meaning |
|---|---|
| `unknown agent "<agent>"` | The agent is not registered on this node |
| `deploy_contract: empty message` | No body, or the body is not JSON |

### `get_contracts`

```
GET /ibc/app/<agent>?action=get_contracts
```

**Reply:** an array of [contract summaries](#contract-summary), one for each contract the agent
has on this node, sorted by contract id.

### `get_contract`

```
GET /ibc/app/<agent>/<contract>?action=get_contract
```

**Reply:** the [contract summary](#contract-summary), or `{}` if the agent has no such contract.

## Calling a contract

### `contract_write`

A transaction: runs a contract function and keeps its changes. The record is added to the
contract's ledger.

```
POST /ibc/app/<agent>/<contract>/<method>?action=contract_write
{"values": {"amount": 5}}
```

| Body field | Meaning |
|---|---|
| `values` | The function's arguments, as [values](#values). Use `{}` for a function without parameters |

**Reply:** a request id.

**Event:** `contract_write`, with `reply` set to the function's result, or the error as a string.
If the function fails, nothing it did is kept, and `state_root` is unchanged.

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
| `"get_partners: not implemented"` | Also `approve_partner` and `get_parameters`: built-in methods that are not defined yet |

**Errors** (request not accepted):

| Message | Meaning |
|---|---|
| `unknown contract "<contract>" of agent "<agent>"` | The agent does not have this contract on this node |

### `contract_read`

Runs a contract function without keeping any change, like `eth_call`. Use it for `view`
functions; any other function can be called too, and its changes are thrown away. Nothing is added
to the ledger, and no event is sent.

```
POST /ibc/app/<agent>/<contract>/<method>?action=contract_read
{"values": {"who": "0x5dad7600C5D89fE3824fFa99ec1c3eB8BF3b0501"}}
```

The body is the same as for `contract_write`. The function sees the latest state, `msg.sender`
set to the agent's address, and `block.timestamp` set to the time of the request.

**Reply:** the function's result, or the error as a string (the same strings as for
`contract_write`). `{"message": "no such contract"}` if the agent has no such contract, or its
deployment has not finished yet.

## Joining a contract

### `join_contract`

Asks a partner of a contract, on the partner's node, to let this agent join. The node forwards the
request (`a2a_connect`) to the partner's node and replies once the partner's node has accepted it.

```
PUT /ibc/app/<agent>?action=join_contract
{"contract": "9f2c…", "address": "http://partner-node:2611", "agent": "alice", "profile": ""}
```

| Body field | Meaning |
|---|---|
| `contract` | The contract's id |
| `address` | The partner's node URL |
| `agent` | The partner's agent id |
| `profile` | Anything the partner should know about the joining agent |

**Reply:** a request id, or `null` if the agent already has the contract on this node.

**Event:** `a2a_reply_join` on the joining agent's stream, when the partner's node answers. `reply`
is `true` if the join was approved. This event has no request id (`"request": ""`) and
`contract: null`.

> In this version joins are never approved (`reply: false`), because `approve_partner` is not
> defined for Solidity contracts yet.

**Errors:**

| Message | Meaning |
|---|---|
| `join_contract needs contract, address and agent` | A body field is missing |
| `PUT <url>: <status>` or a connection error | The partner's node could not be reached or refused the request |

## The event stream

```
GET /stream?agent=<agent>&contract=<contract>[&agent=<agent>&contract=<contract>…]
```

A [Server-Sent Events](https://developer.mozilla.org/en-US/docs/Web/API/Server-sent_events)
stream of the outcomes of asynchronous calls.

- `agent` and `contract` are repeated and paired by position: the first `agent` with the first
  `contract`, and so on.
- An empty `contract` (`contract=`) subscribes to every event of that agent. A contract id
  subscribes to that contract's events only. A missing `contract` counts as empty.
- An event is delivered once if it matches any of the pairs.

The stream starts with an empty message (`data: `), which tells you the subscription is active.
After that every event is one message, and an empty message is sent every 10 seconds to keep the
connection open:

```
data: 

data: {"request":"c690…","contract":"520f…","action":"contract_write","reply":15,"state_root":"0x…","agent":"alice"}

data: 
```

In a browser, `EventSource` handles the stream:

```js
const events = new EventSource('http://localhost:2611/stream?agent=alice&contract=');
events.onmessage = (e) => {
  if (!e.data) return;                 // keep-alive
  const event = JSON.parse(e.data);    // see "Event" below
};
```

If a client falls more than 256 events behind, the node closes its stream. `EventSource`
reconnects by itself, but the events in between are lost. When the node shuts down, it closes
every stream.

## Node to node

These calls are sent by nodes to each other, not by apps. They are listed so that a node can be
implemented or debugged against them.

### `a2a_connect`

A request from another agent to join a contract, sent by its node on `join_contract`.

```
PUT /ibc/app/<partner>/<contract>?action=a2a_connect
{"from": "bob", "to": "alice", "msg": {"pid": "bob", "address": "http://bob-node:2611", "profile": ""}}
```

**Reply:** a request id. **Errors:** `unknown contract …` if the partner does not have the
contract.

The request is added to the contract's ledger. The partner's node decides, sends its answer to the
joining agent's node as `a2a_reply_join`, and sends the partner's own stream an `a2a_connect` event
with `reply` set to the decision.

### `a2a_reply_join`

The answer to a join request.

```
PUT /ibc/app/<joining agent>/<contract>?action=a2a_reply_join
{"from": "alice", "to": "bob", "msg": {"pid": "alice", "address": "http://alice-node:2611", "status": false}}
```

**Reply:** a request id. The joining agent's stream receives an `a2a_reply_join` event with
`reply` set to `status`.

### `a2a_consent`

A message of the consensus protocol. The current protocol uses none.

**Reply:** `{"message": "consensus: not supported by this protocol: a2a_consent"}`.

### `a2a_get_ledger`

A contract's ledger: every record the contract has processed, in order.

```
POST /ibc/app/<agent>/<contract>?action=a2a_get_ledger
{"msg": {"index": 0}}
```

| Body field | Meaning |
|---|---|
| `msg.index` | Return the records from this position on. The first record is 1. `0` or missing: all |

**Reply:** an object that maps each record's position, as a 15-digit string, to the
[record](#record). Sorting the keys as strings gives the ledger's order:

```json
{
  "000000000000001": {"type": "PUT", "action": "deploy_contract", "agent": "alice", "…": "…"},
  "000000000000002": {"type": "POST", "action": "contract_write", "method": "increment", "…": "…"}
}
```

`{"message": "no such contract"}` if the agent has no such contract.

## Objects

### Event

| Field | Meaning |
|---|---|
| `request` | The request id of the call this event answers. Empty for `a2a_reply_join` |
| `contract` | The contract's id, or `null` for `register_agent` and `a2a_reply_join` |
| `action` | `register_agent`, `deploy_contract`, `contract_write`, `a2a_connect` or `a2a_reply_join` |
| `reply` | The outcome: the function's result for `contract_write`, `true`/`false` for the others, or an error string |
| `state_root` | The contract's state root after the call, as a hex hash. All zeros if the contract was never deployed. Absent for `register_agent` and `a2a_reply_join` |
| `agent` | The subscribed agent the event belongs to |

The state root summarizes the contract's entire state. Partners that processed the same records
have the same state root.

### Contract summary

Returned by `get_contracts` and `get_contract`. Fields are present only when they have a value.

| Field | Meaning |
|---|---|
| `id` | The contract's id |
| `name`, `contract`, `protocol`, `default_app`, `profile` | The labels given at deployment |
| `pid`, `address` | The creating agent and its node |
| `code` | The bytecode given at deployment |
| `abi` | The ABI given at deployment |
| `evm_address` | The contract's address in its own EVM state. Every contract an agent deploys gets the same address, since each contract has a state of its own |
| `state_root` | The state root after the last successful write |
| `block_number` | How many records the contract has processed |
| `deploy_error` | Why the deployment failed, if it did |

### Record

A request as the node stores it in a contract's ledger.

| Field | Meaning |
|---|---|
| `type` | The HTTP verb |
| `action` | The action |
| `agent` | The agent in the URL |
| `contract` | The contract's id |
| `method` | The method in the URL, if any |
| `message` | The request body, as sent |
| `timestamp` | When the node received the request: `YYYYMMDDHHMMSSffffff`, UTC, with microseconds |
| `hash_code` | The request id |

## Limits

| What | Limit |
|---|---|
| Request body | 16 MiB |
| Gas per call | 100,000,000 |
| Events a stream client may fall behind | 256 |
| Event stream keep-alive | every 10 seconds |
| Partner request timeout (`join_contract`) | 30 seconds |
