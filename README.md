# ibc

**ibc** (Identity Block Chain) is an implementation of the
[self-sovereign personal digital agent](https://ieeexplore.ieee.org/document/9912178). Every agent
runs on a node it chooses, and agents cooperate through contracts. A contract is shared only among
its *partners*, the agents who joined it, never with a global network, and every contract has its
own isolated state.

This is the Go version of [ibc](https://github.com/ouripoupko/ibc). Contracts run on the **EVM**,
embedded through [go-ethereum](https://github.com/ethereum/go-ethereum), so you write them in
Solidity. A node is a single program with its own embedded database.

> **Work in progress.** A node runs end to end: HTTP API, event stream, contract execution and
> storage. Consensus between partners is not written yet: for now each node executes only the
> records it receives itself, and an agent can use only the contracts it deployed. See
> [what is missing](docs/architecture.md#not-implemented-yet).

## Documentation

| Document | For |
|---|---|
| This page | Running a node |
| [Usage guide](docs/usage.md) | Writing contracts, compiling them, and calling them from the shell or JavaScript |
| [API reference](docs/api.md) | Every HTTP call, its parameters, replies, events and errors |
| [Example contracts](examples/) | Counter, Poll and Board in Solidity, with a shell client and a JavaScript client |
| [Architecture](docs/architecture.md) | How the node works inside, and how to develop it |

## Run a node locally

### 1. Install Go

ibc needs **Go 1.27** or later. Check what you have:

```sh
go version
```

If Go is missing, download it from [go.dev/dl](https://go.dev/dl/):

- **macOS:** run the `.pkg` installer, or `brew install go`.
- **Windows:** run the `.msi` installer.
- **Linux:** unpack the archive into your home directory and put it on your `PATH`. For example,
  for version 1.27.3 on x86-64:

  ```sh
  curl -LO https://go.dev/dl/go1.27.3.linux-amd64.tar.gz
  mkdir -p ~/.local && tar -C ~/.local -xzf go1.27.3.linux-amd64.tar.gz
  echo 'export PATH="$HOME/.local/go/bin:$HOME/go/bin:$PATH"' >> ~/.bashrc
  source ~/.bashrc
  ```

If your Go is older than 1.27 but at least 1.21, it downloads the right version by itself the first
time you build.

### 2. Get the code and build

```sh
git clone https://github.com/ouripoupko/ibc-go.git
cd ibc-go
go build -o ibc ./cmd/ibc
```

The first build downloads the dependencies and takes a minute. The result is a single
program, `ibc`.

### 3. Run

```sh
./ibc -addr 127.0.0.1:2611 -data ~/ibc-data
```

The node is now serving at `http://localhost:2611`. Check it from another terminal:

```sh
curl 'http://localhost:2611/ibc/app/nobody?action=is_exist_agent'
# false
```

`false` means the node is up and has no agent called `nobody`. Continue with the
[usage guide](docs/usage.md) to register an agent and deploy a contract.

To stop the node, press Ctrl-C. It finishes the work it has already accepted, then exits. Your data
stays in the data directory, and the node picks up from there when you start it again.

### Options

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `-addr` | `IBC_ADDR` | `:2611` | Address and port the node listens on |
| `-data` | `IBC_DATA` | `data` | Directory of the node's database |

Some things to know about them:

- **Who can reach the node.** The default `:2611` accepts connections from any machine that can
  reach yours. The node has **no authentication**: anyone who can reach it can act as any agent
  on it. Use `127.0.0.1:2611` to accept only local connections, as in the example above.
- **The data directory** holds everything the node knows: agents, contracts, their state and their
  ledgers. Back it up by copying it while the node is stopped. Only one node can use a data
  directory at a time.
- **The node's address.** When an agent registers, it gives the address at which other nodes reach
  it, for example `http://localhost:2611`. A partner on another machine must be able to reach that
  address.

### Run two nodes on one machine

Give each its own port and data directory:

```sh
./ibc -addr 127.0.0.1:2611 -data ~/ibc-data-1
./ibc -addr 127.0.0.1:2612 -data ~/ibc-data-2
```

### Update

```sh
git pull
go build -o ibc ./cmd/ibc
```

Then restart the node. The data directory is kept. All partners of a contract should run the same
version, because a different version of the EVM could compute different results.

## Without building: `go run`

During development you can skip the separate build step:

```sh
go run ./cmd/ibc -addr 127.0.0.1:2611 -data ~/ibc-data
```

## Project layout

```
cmd/ibc/        the program
node/           wires the parts together
dispatcher/     HTTP API and event stream
consensus/      consensus protocol (a stub for now)
execution/      runs contracts, one goroutine per contract
common/         EVM state, storage, ledger, partner client
examples/       example contracts and clients
docs/           documentation
```

Run the tests with `go test ./...`. See [Architecture](docs/architecture.md#development) for more.
