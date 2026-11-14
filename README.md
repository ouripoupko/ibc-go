# ibc

**ibc** (Identity Block Chain) is an implementation of the
[self-sovereign personal digital agent](https://ieeexplore.ieee.org/document/9912178). Every agent
runs on a node it chooses, and agents cooperate through contracts. A contract is shared only among
its *partners*, the agents who joined it, never with a global network, and every contract has its
own isolated state.

This is the Go version of [ibc](https://github.com/ouripoupko/ibc):

- **Contracts run on the EVM**, embedded through
  [go-ethereum](https://github.com/ethereum/go-ethereum), so you write them in Solidity.
- **Partners agree with [Cordial Miners](docs/consensus.md)**, a Byzantine fault-tolerant
  consensus protocol, run separately for each contract among its partners. A contract may also take
  **guests**, who use it through a partner's node without taking part in consensus. And a user may
  make their copy of a contract **public**: anyone reads it, unsigned, and the contract decides what
  an unknown reader sees.
- **Users sign with a passkey**, a key kept in their phone's or computer's security hardware: no
  wallet to install, and a node never holds it. Apps act for users through their node, which does
  without asking only what the user allowed, per function.
- **Apps send source code**: the user's node compiles it, and the user signs what was compiled.
- **A node is a single program** with its own embedded database.

> **Work in progress.** The essentials work end to end, but parts of the design are still to
> come. See [what is missing](docs/architecture.md#not-implemented-yet).

## Documentation

| Document | For |
|---|---|
| This page | Running a node on your computer |
| [Deploying on a server](docs/deploy.md) | Running a node on a Linux server with HTTPS: service, Caddy, backups, updates |
| [Usage guide](docs/usage.md) | Writing contracts, compiling them, and calling them from the shell or JavaScript |
| [API reference](docs/api.md) | Every HTTP call, its parameters, replies, events and errors |
| [Identity and keys](docs/keys.md) | Passkeys, the root and node keys, app permissions and grants, and how signatures are checked |
| [Example contracts](examples/) | Counter, Poll and Board in Solidity, with a shell client and a JavaScript client |
| [JavaScript client](js/) | The npm package `@ouripoupko/ibc`, for apps and tools |
| [Architecture](docs/architecture.md) | How the node works inside, and how to develop it |
| [Consensus design](docs/consensus.md) | The consensus protocol, Cordial Miners, and how membership changes |

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

### 3. A Solidity compiler

The node compiles contracts from their source, with the Solidity version this release pins
(0.8.38); when you join a contract, it also compiles that contract's source with the version it was
compiled with, and compares. It needs **Node.js**: it downloads solc-js of the version it needs into
its data directory the first time, and runs it with Node.js. Nothing else to install.

Optionally, the native `solc` is faster, and the node uses it if it is the pinned version: download
`solc-static-linux` from the [Solidity releases](https://github.com/ethereum/solidity/releases)
(version 0.8.38), save it as `solc` in a directory on your `PATH`, and make it executable
(`chmod +x`). The node finds it without any option.

Without Node.js (and without solc), the node runs, but cannot compile, and joins contracts with the
warning "source not checked".

### 4. Run

```sh
./ibc -addr 127.0.0.1:2611 -data ~/ibc-data
```

The node is now serving at `http://localhost:2611`. Check it from another terminal:

```sh
curl 'http://localhost:2611/ibc/app?action=node_info'
# {"apiVersion":1,"nodeKey":"0x57E3339B18e36035656375139A5e66B19dc7D60d","storage":…}
```

The reply is the address of the node's key, which the node uses to sign its consensus blocks.

Open **http://localhost:2611** in a browser to use the node's web interface: create your identity
with a passkey, deploy contracts from their source, join contracts, and call their functions. (Passkeys need
`localhost` or `https`; they do not work at `http://127.0.0.1`.)
Continue with the [usage guide](docs/usage.md) for the details, and for the shell and JavaScript
clients.

To run a node on a server, reachable by others over HTTPS, see
[Deploying on a server](docs/deploy.md).

To stop the node, press Ctrl-C. It finishes the work it has already accepted, then exits. Your data
stays in the data directory, and the node picks up from there when you start it again.

### Options

| Flag | Environment variable | Default | Meaning |
|---|---|---|---|
| `-addr` | `IBC_ADDR` | `:2611` | Address and port the node listens on |
| `-data` | `IBC_DATA` | `data` | Directory of the node's database and key |
| `-solc` | `IBC_SOLC` | `solc` | A Solidity compiler command (`solc`, or `solcjs`), which the node uses if it is the pinned version (0.8.38). Otherwise the node downloads solc-js into `-data` and runs it with Node.js |

Some things to know about them:

- **Who can reach the node.** The default `:2611` accepts connections from any machine that can
  reach yours. Every call that changes or shows an agent's data is signed: writes by the agent,
  reads by the reader (the contract decides what each reader may see), and messages between
  nodes by the node's key. Use `127.0.0.1:2611` to accept only local connections, as in the
  example above.
- **The data directory** holds everything the node knows: agents, contracts, their state, their
  ledgers, and the node's key (`nodekey`, readable by you only). Back it up by copying it while
  the node is stopped. Only one node can use a data directory at a time.
- **The node key** is created on the first start and identifies the node in consensus. Agents
  authorize it when they register. It is not anyone's personal key.
- **The node's address.** When an agent registers, it gives the address at which other nodes reach
  it, for example `http://localhost:2611`. Partners on other machines must be able to reach that
  address: their nodes exchange consensus messages with yours.

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
cmd/ibc/              the program (node, key and signing tools)
node/                 wires the parts together
dispatcher/           HTTP API and event stream
web/                  the web interface, built into the program
js/                   the JavaScript client (npm package @ouripoupko/ibc), in TypeScript
consensus/            each contract's protocol; Cordial Miners in consensus/cordial
execution/            runs contracts, one goroutine per contract
identity/             signed transactions and registrations (EIP-712)
common/               EVM state, storage, ledger, outbox
examples/       example contracts and clients
docs/           documentation
```

Run the tests with `go test ./...`. See [Architecture](docs/architecture.md#development) for more.

## License

ibc is free software under the [MIT License](LICENSE): anyone may use, copy, change and share it,
for any purpose, as long as the copyright notice stays with the copies.

The `ibc` program includes [go-ethereum](https://github.com/ethereum/go-ethereum), which has its
own license, the [LGPL-3.0](https://www.gnu.org/licenses/lgpl-3.0.html). If you distribute a
built `ibc` program, make go-ethereum's source code available to those you give it to, as that
license asks.
