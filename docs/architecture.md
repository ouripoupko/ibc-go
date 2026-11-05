# Architecture

How an ibc node works inside, and how to develop it. For running a node see the
[README](../README.md); for using it, the [usage guide](usage.md).

- [Overview](#overview)
- [Packages](#packages)
- [The life of a record](#the-life-of-a-record)
- [Storage](#storage)
- [Contracts on the EVM](#contracts-on-the-evm)
- [Development](#development)
- [Not implemented yet](#not-implemented-yet)

## Overview

This is a Go rewrite of the Python [ibc](https://github.com/ouripoupko/ibc). The Python node ran
three processes (a Flask app under gunicorn, a consensus service and an execution service),
connected through Redis, and kept its data in MongoDB. Contracts were Python classes run with
`exec`.

The Go node is one process. The three parts are goroutines connected by function calls, the data
is in an embedded [Pebble](https://github.com/cockroachdb/pebble) database, and contracts run on
the EVM from [go-ethereum](https://github.com/ethereum/go-ethereum).

```
HTTP (dispatcher)
  ├─ reads ─────────────────────────────► execution: read-only call on the latest state
  ├─ register_agent ────────────────────► execution
  └─ deploy / write / join / protocol ──► consensus (stub: accepts at once)
                                              │ committed records
                                              ▼
                           execution.Navigator.Submit
                             1. append the record to the contract's ledger (durable)
                             2. wake the contract's goroutine
                                              │
                             one goroutine per contract: apply every ledger record
                             after the contract's cursor, in order, on its EVM state
                                              │
                    replies ──► event stream (/stream)     join outcomes ──► consensus
```

## Packages

| Package | Python counterpart | Role |
|---|---|---|
| [`cmd/ibc`](../cmd/ibc) | `ibc.py`, `*_service.py` | The program: flags, signals |
| [`node`](../node) | `docker-start.sh` | Wires storage, execution, consensus and HTTP in one process; startup recovery; orderly shutdown |
| [`dispatcher`](../dispatcher) | `ibc.py`, `dispatcher/` | HTTP API, Server-Sent Events, the in-process event broker that replaces Redis pub/sub |
| [`consensus`](../consensus) | `consensus/` | The `Protocol` interface and the `Immediate` stub |
| [`execution`](../execution) | `execution/` | `Navigator` (per-contract goroutines, recovery, read-only queries), `ContractExecution`, records |
| [`common`](../common) | `common/` | `State` (the EVM), the ABI codec, documents, ledgers, the partner HTTP client and outbox |

## The life of a record

1. **The dispatcher** turns an HTTP request into a record: the verb, the action, the agent,
   contract and method from the URL, the body, a UTC timestamp, and `hash_code`, the SHA-256 of the
   record's JSON.
2. **Consensus** decides when the record is committed. The current `Immediate` stub commits every
   record at once, in arrival order, without talking to any partner. A real protocol will run
   agreement among the contract's partners first, and will implement the same
   [`consensus.Protocol`](../consensus/consensus.go) interface.
3. **`Navigator.Submit`** appends the committed record to its contract's ledger before it returns.
   From that point, a crash cannot lose the record. A record can carry an `index`, its position in
   the ledger as assigned by consensus. A record whose index is already in the ledger is ignored,
   so consensus can deliver it again safely.
4. **The contract's goroutine** applies, in order, every ledger record after the contract's
   *cursor*. Each contract has its own goroutine, so contracts never wait for each other. A
   goroutine closes after 60 seconds without records and starts again on demand.
5. **The outcome** goes to the event stream as a reply, and the outcome of a join also goes back to
   consensus.

### The cursor

A contract's cursor is its `block_number`: the position of the last ledger record applied. Every
record in the ledger advances it exactly once, whatever happens: a successful write, a revert, a
call to a built-in stub, a join, or a record for a contract whose deployment failed. Only a local
storage failure leaves the cursor in place, and the record is applied again later.

Because of this, the node can always tell which records it still has to apply. At startup,
`Navigator.Recover` wakes every contract whose ledger is ahead of its cursor. Applying a record is
deterministic, so applying it again after a crash reaches the same state root.

A write is committed in this order: the EVM state's trie nodes first, then the contract document
with the new root and cursor, in one atomic write. A crash between the two leaves the document at
the old root, and the record is applied again.

### Reads

`contract_read` does not go through consensus or the contract's goroutine. It opens the contract's
state at its latest root and runs the call on a copy that is thrown away. A read never deploys a
contract: a contract whose deploy record has not been applied yet is reported as unknown.

## Storage

One Pebble database holds everything, under separate key prefixes:

| Prefix | Contents |
|---|---|
| `ibc/doc/` | Documents, in the layout of the Python MongoDB: `agents/<agent>` → `contracts/<hash>` → `pda_partners/<pid>` |
| `ibc/ledger/` | One ledger per agent and contract, numbered from 1 |
| `ibc/evm/` | Each contract's EVM world state (trie nodes and code), separately for every agent and contract |

Variable-length parts of a key are length-prefixed, so no id can reach into another id's keys,
whatever characters it contains.

Each document is one JSON value under one key, so updating a document is atomic. Numbers are read
back as `json.Number`, so large integers stay exact.

The `Document` and `Collection` interfaces in [`common/document.go`](../common/document.go) keep
the storage replaceable. Tests use in-memory implementations.

Pebble is a library compiled into the program, not a separate server. The data directory holds its
write-ahead log and data files, and only one process can open it at a time.

## Contracts on the EVM

### State

Every contract instance has its own world state: its own `state.StateDB` and trie. There is no
state shared between contracts. After every successful write, the state root is stored in the
contract document and reported in the event. Partners that processed the same records have the
same root, which is what a consensus protocol can compare.

### Identity

An agent id maps to an EVM address: `common.BytesToAddress(crypto.Keccak256([]byte(pid))[12:])`
(`common.AgentAddress`). That address is `msg.sender` for the agent's calls.

### Determinism

Every partner must compute the same result for the same records, so nothing depends on the node:

| Block field | Value |
|---|---|
| `block.timestamp` | The record's timestamp (`YYYYMMDDHHMMSSffffff`, UTC), in unix seconds |
| `block.number` | The contract's cursor |
| `coinbase`, `prevrandao`, `basefee`, `blobbasefee`, `gasprice`, `blockhash` | Zero |
| `block.gaslimit` | 100,000,000 for every call. Gas only bounds runaway code; nobody pays |
| `chainid` | `0x1bc` |
| Value transfers | Always zero |
| Precompiles | None |

Every fork up to and including **Osaka** is active from genesis. Amsterdam and later are still in
development in go-ethereum and stay off.

### Arguments and results

[`common/abi_codec.go`](../common/abi_codec.go) converts between JSON values and the Go types of
go-ethereum's `accounts/abi`. The rules are in the [API reference](api.md#values). Revert data is
decoded as `Error(string)`, `Panic(uint256)`, or a custom error from the contract's ABI.

### Differences from the Python contract model

- Contracts are EVM bytecode with an ABI, not Python classes.
- A failed call rolls back completely. In Python, writes made before an exception stayed.
- A read runs on a copy of the state. In Python, a read could change state.
- Dropped: `read()` between contracts, governance `parameters` and medians, `random`, `hashcode`,
  `elapsed_time`, and `Storage(name)`.
- The built-in methods `get_partners`, `approve_partner` and `get_parameters` are stubs that answer
  `not implemented`.

## Development

### Tests

```sh
go test ./...        # all tests
go test -race ./...  # with the race detector
go vet ./...
```

The tests include:

- the EVM state: deployment, writes, reads, reverts, determinism between two partners, restarts;
- storage and the ledger on Pebble;
- the navigator: ordering, redelivery, crash recovery, idle shutdown;
- end-to-end runs over real HTTP: the API walkthrough, two nodes joining across the network,
  stream scoping, and shutdown with open streams;
- [`examples/examples_test.go`](../examples/examples_test.go), which runs the calls shown in the
  [usage guide](usage.md) against the example contracts, so the documented results stay true.

### Contracts used by the tests

[`testdata/Registry.sol`](../testdata/Registry.sol) is the contract of the unit tests. Its compiled
`Registry.bin` and `Registry.abi` are committed, so the tests do not need solc. To rebuild them:

```sh
npx -p solc@0.8.38 solcjs --bin --abi --optimize --base-path testdata -o out testdata/Registry.sol
mv out/Registry_sol_Registry.bin testdata/Registry.bin
mv out/Registry_sol_Registry.abi testdata/Registry.abi
```

The example contracts are rebuilt with `examples/scripts/compile.sh`.

### Upgrading go-ethereum

go-ethereum is pinned in `go.mod`. Its internal APIs (`vm.NewEVM`, `state.New`, `triedb`, gas
accounting) change between releases, so check the signatures in the module cache of the new
version, not in older documentation. A new version can also change results, so all partners must
upgrade together.

## Not implemented yet

- **Consensus.** `consensus.Immediate` commits every record at once and talks to no partner, so
  partners do not see each other's writes. When a join is approved, Python fetched the partner's
  ledger and replayed it; syncing a joining partner belongs to the new protocol too. The Python
  version chose a protocol per contract by the deploy message's `protocol` field, which is ignored
  for now.
- **Joins.** `approve_partner` is a stub, so every join is answered "not approved", and only the
  creator of a contract can use it.
- **Built-in methods.** `get_partners`, `approve_partner` and `get_parameters` need semantics for
  Solidity contracts.
- **Partners in the state root.** The partner list is a document, outside the EVM state, so it is
  not covered by the state root, and a crash in the middle of an approved join could change the
  replayed outcome.
- **Duplicates after recovery.** Records applied again after a crash publish their replies again
  and resend join replies; clients and partners must tolerate duplicates.
- **Contract events.** Logs emitted by contracts are discarded.
- **Precompile addresses.** There are no precompiles, but a call to one of their addresses does not
  fail: `ecrecover` and `ripemd160` return zero, `sha256` reverts.
- **Execution rules version.** Records do not yet say which execution rules (EVM fork, gas limit,
  block context) interpret them.
- **Authentication.** Anyone who can reach a node can act as any agent on it.
- **Packaging.** No Dockerfile or compose file yet.
