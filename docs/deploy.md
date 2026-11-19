# Deploying a node on a server

This guide puts an ibc node on a small Linux server, reachable by everyone at an HTTPS address such
as `https://ibc.example.com`. It uses a [Hetzner Cloud](https://www.hetzner.com/cloud) server as the
example, but any Linux server works the same way.

When you are done:

- the node runs as a service, starts at boot, and restarts if it stops;
- [Caddy](https://caddyserver.com) receives the HTTPS traffic, with a certificate it gets and renews
  by itself, and passes it to the node;
- the node itself listens only inside the server;
- the node's data is backed up.

```
browser / other nodes ──HTTPS:443──► Caddy ──HTTP──► ibc on 127.0.0.1:2611 ──► /var/lib/ibc
```

Why HTTPS: passkeys work only on secure pages: `https://` (or `localhost`). Other nodes also reach yours at this address.

**Choose the address for good.** Your users' passkeys belong to the node's address
(`ibc.example.com`): the phone lets only that site use them. If the node moves to another address,
its users cannot use their identity there. The node is its users'
representation on the internet; give it a name you will keep.

- [1. What you need](#1-what-you-need)
- [2. Create the server](#2-create-the-server)
- [3. Point your domain at it](#3-point-your-domain-at-it)
- [4. Prepare the server](#4-prepare-the-server)
- [5. Install ibc](#5-install-ibc)
- [6. Check the service](#6-check-the-service)
- [7. Add HTTPS with Caddy](#7-add-https-with-caddy)
- [8. Use it](#8-use-it)
- [9. Backups](#9-backups)
- [10. Updates](#10-updates)
- [11. Day to day](#11-day-to-day)
- [12. Troubleshooting](#12-troubleshooting)

## 1. What you need

- **A server.** 2 vCPUs, 4 GB of memory and 40 GB of disk are plenty for testing and for modest
  use: ibc is one program with an embedded database.
- **A domain name** you control, for example `example.com`. You will give the node a name under it,
  such as `ibc.example.com`. A certificate cannot be issued for a bare IP address.
- **An SSH key** on your computer (`~/.ssh/id_ed25519.pub`). Create one with `ssh-keygen -t ed25519`
  if you have none.

In the commands below, replace `ibc.example.com` with your node's name and `SERVER_IP` with your
server's address.

## 2. Create the server

In the Hetzner Cloud console, create a server:

| Setting | Choice |
|---|---|
| Location | Near you and your partners |
| Image | Ubuntu 24.04 |
| Type | The smallest shared-vCPU type with 2 vCPUs and 4 GB, either x86 (Intel/AMD) or Arm (Ampere). Arm is usually cheaper; ibc runs on both |
| Networking | Public IPv4 and IPv6 |
| SSH keys | Add yours, so you log in with it and not a password |
| Backups | Turn on (see [Backups](#9-backups)); it adds a small monthly cost |
| Firewall | Create one that allows incoming **TCP 22** (SSH), **80** and **443** (web), and nothing else |

Note whether you chose **x86** or **Arm**: you install the package for that architecture in step 5.

When the server is created, the console shows its IPv4 address (and IPv6 address).

## 3. Point your domain at it

At the company that manages your domain's DNS, add records for the node's name:

| Type | Name | Value |
|---|---|---|
| `A` | `ibc` | the server's IPv4 address |
| `AAAA` | `ibc` | the server's IPv6 address (optional) |

Check that the name resolves to the server before you continue; it can take a few minutes:

```sh
dig +short ibc.example.com      # prints the server's IPv4 address
```

## 4. Prepare the server

Log in as root:

```sh
ssh root@SERVER_IP
```

Update the system, and let it install security updates by itself from now on:

```sh
apt update && apt upgrade -y
apt install -y unattended-upgrades
dpkg-reconfigure -plow unattended-upgrades     # answer "Yes"
```

Check that the clock is synchronized. Signed reads are accepted for 60 seconds and messages between
nodes for 5 minutes, so a wrong clock makes the node refuse them:

```sh
timedatectl          # "System clock synchronized: yes"
```

If you did not create a Hetzner firewall in step 2, use the server's own firewall instead:

```sh
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw enable
```

## 5. Install ibc

ibc is released as a Debian package, for x86 (`amd64`) and Arm (`arm64`), on the project's
[releases page](https://github.com/ouripoupko/ibc-go/releases). On the server, download the newest
one and install it:

```sh
ARCH=$(dpkg --print-architecture)      # amd64 (x86) or arm64 (Arm)
curl -LO https://github.com/ouripoupko/ibc-go/releases/latest/download/ibc_$ARCH.deb
apt install -y ./ibc_$ARCH.deb
ibc version
```

The package:

- installs the program as `/usr/bin/ibc`; the web interface is built into it;
- creates the `ibc` user, which cannot log in, and owns only the node's data in `/var/lib/ibc`;
- installs the Solidity compiler (below), and Node.js;
- installs the `ibc` service, which starts now and at boot, and restarts if it stops;
- keeps the service's settings in `/etc/default/ibc`.

To check a download first, compare it with the release's `SHA256SUMS` file:
`curl -LO https://github.com/ouripoupko/ibc-go/releases/latest/download/SHA256SUMS && sha256sum --check --ignore-missing SHA256SUMS`.

**The Solidity compiler.** Apps send contracts as source, and the node compiles them; when one of
your agents joins a contract, the node also compiles that contract's source again and compares it
with its code. The package brings the compiler the node pins (solc 0.8.38 in this release), as
`/usr/lib/ibc/solc`; there is nothing to install. A contract compiled with another version of
Solidity needs that version: the node downloads it by itself from `https://binaries.soliditylang.org`
(solc-js, checked against Solidity's published hash) into `/var/lib/ibc/solc`, and runs it with
Node.js, which the package also installs.

**Building it yourself.** If you develop ibc, you can build the program on your computer for the
server's architecture and install it over the package's:

```sh
# x86 server (for Arm: GOARCH=arm64):
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o ibc-linux ./cmd/ibc
scp ibc-linux root@SERVER_IP:/tmp/ibc
ssh root@SERVER_IP 'install -m 755 /tmp/ibc /usr/bin/ibc && rm /tmp/ibc && systemctl restart ibc'
```

The next package you install replaces it.

## 6. Check the service

```sh
systemctl status ibc                 # "active (running)"
```

Check that it answers, from the server itself:

```sh
curl -s 'http://127.0.0.1:2611/ibc/app?action=node_info'
# {"nodeKey":"0x57E3339B18e36035656375139A5e66B19dc7D60d"}
```

On its first start the node created its key, `/var/lib/ibc/nodekey`. The address above is that
key's: it identifies your node in consensus. It is not anyone's personal key.

The service listens inside the server only, on `127.0.0.1:2611`; Caddy passes the traffic in (next
step). Its settings are in `/etc/default/ibc`:

| Setting | Default | Meaning |
|---|---|---|
| `IBC_ADDR` | `127.0.0.1:2611` | Where the node listens |
| `IBC_DATA` | `/var/lib/ibc` | The node's database and key; the service owns this directory, so keep it |
| `IBC_SOLC` | `/usr/lib/ibc/solc` | The Solidity compiler of the pinned version, which the package installs |

After changing one, run `systemctl restart ibc`. Package updates keep your changes.

## 7. Add HTTPS with Caddy

Install Caddy. Ubuntu's own package works:

```sh
apt install -y caddy
```

For the newest version, use Caddy's own package repository instead; its
[install page](https://caddyserver.com/docs/install#debian-ubuntu-raspbian) has the commands.

Replace the contents of `/etc/caddy/Caddyfile` with:

```
ibc.example.com {
	reverse_proxy 127.0.0.1:2611
}
```

Load it:

```sh
systemctl reload caddy
```

Caddy now gets a certificate for `ibc.example.com` from Let's Encrypt, which needs ports 80 and 443
open and the DNS record from step 3. It renews the certificate by itself. Caddy also passes the
node's event stream through as it comes, which the web interface needs.

From your computer, check:

```sh
curl -s 'https://ibc.example.com/ibc/app?action=node_info'
```

If this fails, see [Troubleshooting](#12-troubleshooting).

## 8. Use it

Open `https://ibc.example.com` in a browser and create your identity: your passkey is made for
this address. Everything in the [usage guide](usage.md) now works against it.

- **Register at the HTTPS address.** When an agent registers, the page records the address it was
  opened at as the agent's node. Partners' nodes reach yours there, so always use
  `https://ibc.example.com`, not the IP address.
- **The shell client** works the same way: `export IBC_URL=https://ibc.example.com`.
- **One node serves many agents.** Each person creates their own identity, with their own passkey.
- **Partners on other servers** run their own nodes. Each node must be reachable at the address its
  agents registered.

## 9. Backups

Everything the node knows is in `/var/lib/ibc`: agents, contracts, their state and ledgers, and the
node key `nodekey`. Back it up.

**Hetzner's backups** (step 2) copy the whole server daily while it runs. Such a copy is like the
state after a power cut: the database is built to recover from that, and the node picks up where it
was. Restore a backup from the Hetzner console.

**A clean copy** of the data only, for example before an update or to keep elsewhere. The node is
stopped for a few seconds:

```sh
systemctl stop ibc
tar -czf /root/ibc-backup-$(date +%F).tar.gz -C /var/lib ibc
systemctl start ibc
```

Then copy it off the server, from your computer:

```sh
scp root@SERVER_IP:/root/ibc-backup-*.tar.gz .
```

**To restore** a clean copy:

```sh
systemctl stop ibc
rm -rf /var/lib/ibc
tar -xzf ibc-backup-2026-11-06.tar.gz -C /var/lib
chown -R ibc:ibc /var/lib/ibc
systemctl start ibc
```

Keep backups private: they hold the node key and the contracts' data. If the node key is lost, the
node cannot sign for its agents any more. Each agent can then move its contracts to a new node with a
`rotate_key` transaction, signed with its passkey (see the [API reference](api.md#rotate_key)).

## 10. Updates

On the server, download the newest package and install it, as in step 5. Installing restarts the
service with the new program:

```sh
ARCH=$(dpkg --print-architecture)
curl -LO https://github.com/ouripoupko/ibc-go/releases/latest/download/ibc_$ARCH.deb
apt install -y ./ibc_$ARCH.deb
ibc version
```

For a specific version, not the newest, download from that release instead, for example
`https://github.com/ouripoupko/ibc-go/releases/download/v0.1.0/ibc_$ARCH.deb`.

The data and your settings in `/etc/default/ibc` are kept. A clean backup first is a good habit.

All partners of a contract must run the same version: a new version can change how transactions are
executed, and partners must compute the same results. Agree on when to update together.

## 11. Day to day

| Task | Command |
|---|---|
| Is it running? | `systemctl status ibc` |
| Which version? | `ibc version` |
| Follow the node's log | `journalctl -u ibc -f` |
| The log since this morning | `journalctl -u ibc --since today` |
| Restart | `systemctl restart ibc` |
| Caddy's log (certificates, requests) | `journalctl -u caddy -f` |
| Disk space | `df -h /var/lib/ibc` and `du -sh /var/lib/ibc`; the web page's footer also shows the data folder's size |
| Log space (in the system journal, shared with other services) | `journalctl --disk-usage` |

**Who can see what.** The node stores every contract its agents are members of, so whoever runs the
server (you, and Hetzner as your host) can read that data on disk. Visitors cannot: every read
through the node is signed, and contracts decide what each reader may see. Tell the people who use
your node.

## 12. Troubleshooting

| Problem | Likely cause |
|---|---|
| Caddy cannot get a certificate (`journalctl -u caddy`) | The DNS record does not point at the server yet, or ports 80 and 443 are closed in the firewall |
| `curl` from outside hangs (`Connection timed out`), while it answers on the server | The firewall drops ports 80 and 443. Hetzner's default firewall allows only 22 and ICMP: add inbound TCP 80 and 443 (step 2), and check it is applied to the server |
| `TLS alert, internal error` | Caddy has no certificate yet. If the ports were closed when it first tried, it waits before trying again: `systemctl restart caddy`, then look for "certificate obtained successfully" in `journalctl -u caddy` |
| `502 Bad Gateway` | ibc is not running: `systemctl status ibc`, `journalctl -u ibc` |
| The page says passkeys need a secure page | The page is open at `http://`, not `https://` |
| "This passkey is not an ibc identity on this node" when signing in | The passkey was made on another node's address |
| "solc …: running solc-js needs Node.js" | Node.js is missing; the package installs it: `apt install -y nodejs` |
| "solc …: …" with a network error | A contract needs another Solidity version, and the server cannot reach `https://binaries.soliditylang.org` to download it |
| `query expired, or its time is wrong` | The clock of the server or of the user's computer is wrong: `timedatectl` |
| Partners never see your transactions, or a join never completes | Your node is not reachable at the address your agents registered, or theirs is not. Check with `curl https://<their node>/ibc/app?action=node_info` from each side |
| The service does not start after an update | `journalctl -u ibc -e` shows why; install the previous release's package, or restore a backup |
