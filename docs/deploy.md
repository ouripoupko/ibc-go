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
- [5. Build and copy the program](#5-build-and-copy-the-program)
- [6. Run ibc as a service](#6-run-ibc-as-a-service)
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
- **Go** on your computer, to build the program (see the [README](../README.md#1-install-go)).

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

Note whether you chose **x86** or **Arm**: you build the program for that architecture in step 5.

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

Create a user for the node. It cannot log in, and owns only the node's data:

```sh
useradd --system --home-dir /var/lib/ibc --shell /usr/sbin/nologin ibc
```

If you did not create a Hetzner firewall in step 2, use the server's own firewall instead:

```sh
ufw allow OpenSSH
ufw allow 80/tcp
ufw allow 443/tcp
ufw enable
```

## 5. Build and copy the program

On **your computer**, in the project directory, build the program for the server's architecture.
`CGO_ENABLED=0` makes a self-contained program that needs nothing else installed on the server:

```sh
# x86 server:
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o ibc-linux ./cmd/ibc
# Arm server:
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -o ibc-linux ./cmd/ibc
```

Copy it to the server, and install it as `/usr/local/bin/ibc`:

```sh
scp ibc-linux root@SERVER_IP:/tmp/ibc
ssh root@SERVER_IP 'install -m 755 /tmp/ibc /usr/local/bin/ibc && rm /tmp/ibc'
```

The web interface is built into the program; there are no other files to copy.

**The Solidity compiler.** Apps send contracts as source, and the node compiles them; when one of
your agents joins a contract, the node also compiles that contract's source again, with the version
it was compiled with, and compares it with its code. This release of ibc pins **solc 0.8.38**. The
node gets the compilers it needs by itself: it downloads solc-js (the Solidity compiler built for
JavaScript) of the version it needs from soliditylang.org into its data directory
(`/var/lib/ibc/solc`), checks it against Solidity's published hash, and runs it with **Node.js**. So
install Node.js, on x86 and Arm servers alike:

```sh
ssh root@SERVER_IP
apt install -y nodejs
node --version
```

The server must also reach `https://binaries.soliditylang.org`. Without Node.js the node cannot
compile, and it joins contracts with the warning "source not checked".

On x86, the native `solc` is faster, and the node uses it for its pinned version if it is installed
(the service below names it): the static build, one file, from the
[Solidity releases](https://github.com/ethereum/solidity/releases):

```sh
curl -L -o /usr/local/bin/solc https://github.com/ethereum/solidity/releases/download/v0.8.38/solc-static-linux
chmod 755 /usr/local/bin/solc
solc --version
```

On Arm, skip this: there is no official Arm build, and the node uses solc-js.

## 6. Run ibc as a service

On the server, create the file `/etc/systemd/system/ibc.service`:

```ini
[Unit]
Description=ibc node
After=network-online.target
Wants=network-online.target

[Service]
User=ibc
Group=ibc
ExecStart=/usr/local/bin/ibc
# Listen inside the server only; Caddy passes the traffic in.
Environment=IBC_ADDR=127.0.0.1:2611
Environment=IBC_DATA=/var/lib/ibc
# The native Solidity compiler, if installed (step 5); otherwise the node downloads solc-js.
Environment=IBC_SOLC=/usr/local/bin/solc
# Creates /var/lib/ibc, owned by the ibc user.
StateDirectory=ibc
StateDirectoryMode=0700
Restart=on-failure
RestartSec=5
# On stop, the node finishes the work it accepted; give it time.
TimeoutStopSec=60

# Limit what the program may touch.
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
PrivateDevices=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true

[Install]
WantedBy=multi-user.target
```

Start it, and make it start at boot:

```sh
systemctl daemon-reload
systemctl enable --now ibc
systemctl status ibc                 # "active (running)"
```

Check that it answers, from the server itself:

```sh
curl -s 'http://127.0.0.1:2611/ibc/app?action=node_info'
# {"nodeKey":"0x57E3339B18e36035656375139A5e66B19dc7D60d"}
```

On its first start the node created its key, `/var/lib/ibc/nodekey`. The address above is that
key's: it identifies your node in consensus. It is not anyone's personal key.

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

Build the new version on your computer as in step 5, then copy it and restart:

```sh
scp ibc-linux root@SERVER_IP:/tmp/ibc
ssh root@SERVER_IP 'install -m 755 /tmp/ibc /usr/local/bin/ibc && rm /tmp/ibc && systemctl restart ibc'
```

The data is kept. A clean backup first is a good habit.

All partners of a contract must run the same version: a new version can change how transactions are
executed, and partners must compute the same results. Agree on when to update together.

## 11. Day to day

| Task | Command |
|---|---|
| Is it running? | `systemctl status ibc` |
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
| "solc 0.8.38: running solc-js needs Node.js" | Install Node.js (step 5) |
| "solc 0.8.38: …" with a network error | The server cannot reach `https://binaries.soliditylang.org` to download the compiler |
| `query expired, or its time is wrong` | The clock of the server or of the user's computer is wrong: `timedatectl` |
| Partners never see your transactions, or a join never completes | Your node is not reachable at the address your agents registered, or theirs is not. Check with `curl https://<their node>/ibc/app?action=node_info` from each side |
| The service does not start after an update | `journalctl -u ibc -e` shows why; restore the previous program, or a backup |
