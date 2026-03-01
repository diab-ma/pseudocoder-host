<p align="center">
  <img src="assets/github-banner.svg" alt="pseudocoder" width="600">
</p>

<p align="center">
  <a href="LICENSE">
    <img src="https://img.shields.io/badge/license-MIT-yellow.svg" alt="MIT License">
  </a>
</p>

supervise terminal coding sessions from your phone. watch terminal output, review code changes, and accept or reject diffs anytime, anywhere.

> **WARNING:**
> 
> THIS APP GRANTS FULL TERMINAL ACCESS TO PAIRED DEVICES. COMMANDS RUN WITH YOUR USER PRIVILEGES. **ONLY USE OVER TAILSCALE OR A TRUSTED LOCAL NETWORK. NEVER EXPOSE TO THE PUBLIC INTERNET.**

## Prerequisites

- [Tailscale](https://tailscale.com/download) installed and running on phone and host machine
- iphone with iOS 15+

## Installation

### macOS

```bash
# install Tailscale
brew install tailscale && brew services start tailscale

# connect to Tailscale network
sudo tailscale up

# install pseudocoder
brew tap diab-ma/pseudocoder-host https://github.com/diab-ma/pseudocoder-host
brew install pseudocoder
```

### Linux

```bash
# install Tailscale
curl -fsSL https://tailscale.com/install.sh | sh

# connect to Tailscale network
sudo tailscale up

# install pseudocoder
curl -sSL https://raw.githubusercontent.com/diab-ma/pseudocoder-host/main/install.sh | bash
```

### Windows (WSL2)

run inside WSL:

```bash
# install Tailscale
curl -fsSL https://tailscale.com/install.sh | sh

# start Tailscale daemon
sudo tailscaled &

# connect to Tailscale network
sudo tailscale up

# install pseudocoder
curl -sSL https://raw.githubusercontent.com/diab-ma/pseudocoder-host/main/install.sh | bash
```

also install Tailscale on your phone and sign in with the same account.

---

## Quick Start

1. get the mobile app (check your email for TestFlight invite)
2. note your Tailscale IP: `tailscale ip -4` (starts with `100.`)
3. start the host:
   ```bash
   cd /path/to/your/project    # navigate to your project
   pseudocoder start --pair --qr  # start host and show pairing QR
   ```
4. scan the QR code from the app and tap **Trust**

---

## Usage

<h3 align="center">Sessions</h3>

create multiple terminal sessions, easily switch via the session pill, and attach to an existing tmux session to continue right where you left off.

<p align="center">
  <img src="assets/screenshot-new-session.png" alt="New Session" width="300">
</p>

<h3 align="center">Terminal</h3>

real-time terminal output streaming. send terminal commands, monitor CLI tools as they work, and accept changes from wherever you are.

<p align="center">
  <img src="assets/screenshot-terminal.png" alt="Terminal" width="300">
</p>

<h3 align="center">Review Cards</h3>

commit panel in review tab allows a frictionless way to enter message, commit, and push. track all your accepted/rejected changes and easily undo. nearby chunks are grouped by proximity so related changes stay together.

- **Accept** stages changes (`git add`)
- **Reject** restores original (`git restore`)
- **Undo** reverts decision (`git restore --staged` or `git checkout`)
- **Commit** creates a commit (`git commit`)
- **Push** pushes to remote (`git push`)

<p align="center">
  <img src="assets/screenshot-review.png" alt="Review and Commit" width="300">
</p>

<h3 align="center">File Browser</h3>

browse, read, write, create, and delete files in the supervised repository from mobile. write operations use version-conflict detection to prevent overwriting concurrent changes. external file changes are detected automatically via polling.

<h3 align="center">Git</h3>

paginated commit history, branch list with create and switch (blocks switching with a dirty tree), fetch, pull (fast-forward-only), and commit/push with request-scoped correlation. all git operations are scoped to the supervised repository.

<h3 align="center">Keep-Awake</h3>

prevent host machine from sleeping during active sessions. the host runs an authoritative state machine with multi-client lease arbitration and battery policy enforcement (macOS). enable via CLI or startup flag:

```bash
pseudocoder keep-awake enable-remote    # enable remote keep-awake policy
pseudocoder keep-awake disable-remote   # disable remote keep-awake policy
pseudocoder keep-awake status           # show current policy status
pseudocoder start --enable-remote-keep-awake  # enable at startup
```

---

## Diagnostics

run `pseudocoder doctor` to diagnose onboarding readiness and connectivity issues. checks TLS certificates, network reachability, pairing IPC socket, and host readiness. use `--json` for machine-readable output.

```bash
pseudocoder doctor          # interactive preflight checks
pseudocoder doctor --json   # JSON output for scripting
```

---

## Security

use Tailscale or trusted LAN only. we recommend never exposing to public internet.
all connections use TLS 1.2+. paired devices have full terminal access. only pair devices you control.
pairing codes are generated locally (IPC/loopback) to avoid exposing them on the network.
file operations are scoped to the supervised repository directory.
keep-awake policy mutations are restricted to loopback-only API with Bearer-token auth.

manage devices:
```bash
pseudocoder devices list          # view paired devices
pseudocoder devices revoke <id>   # remove access
```

---

## Configuration

`pseudocoder start` creates `~/.pseudocoder/config.toml` if missing. optional settings:

- `pair_socket`: local IPC socket used by `pseudocoder pair` (default: `~/.pseudocoder/pair.sock`)
- `chunk_grouping_enabled`: group nearby diff hunks into a single review card section
- `chunk_grouping_proximity`: max line distance for grouping (default: `20` when enabled)
- `keep_awake_remote_enabled`: allow mobile devices to activate keep-awake (default: `false`)
- `keep_awake_allow_on_battery`: permit keep-awake when on battery power (default: `true`)
- `keep_awake_auto_disable_battery_percent`: battery % threshold to auto-disable (0 = disabled)
- `keep_awake_audit_max_rows`: max durable audit log rows retained (default: `1000`)

---

## Troubleshooting

| problem | solution |
|---------|----------|
| connection refused | use Tailscale IP (`100.x.x.x`), check `tailscale status` |
| certificate errors | tap **Trust** on first connection, or re-pair |
| pairing code expired | run `pseudocoder pair --qr` (codes last 5 min) |
| `pseudocoder pair` fails | ensure the host is running and `~/.pseudocoder/pair.sock` is accessible |
| QR won't scan | enter manually: host IP, port `7070`, 6-digit code |
| keep-awake not activating | check `pseudocoder keep-awake status`, ensure `keep_awake_remote_enabled = true` in config |
| file write conflict | another process modified the file; re-read and retry the write |
| branch switch blocked | commit or stash uncommitted changes before switching branches |
| pull rejected | non-fast-forward; fetch and merge or rebase locally |
| general connectivity issues | run `pseudocoder doctor` for guided diagnostics |
| macOS Gatekeeper | `xattr -d com.apple.quarantine /usr/local/bin/pseudocoder` |

---

## Command Reference

| command | description |
|---------|-------------|
| `pseudocoder start --pair --qr` | start host with mobile-ready defaults + pairing QR |
| `pseudocoder host start` | start host daemon with advanced options |
| `pseudocoder host status` | show host daemon status |
| `pseudocoder pair --qr` | generate new pairing code |
| `pseudocoder doctor` | diagnose onboarding readiness and connectivity |
| `pseudocoder doctor --json` | diagnostics with JSON output |
| `pseudocoder session list` | list active sessions |
| `pseudocoder session new --name <name>` | create a new session |
| `pseudocoder session kill <id>` | kill a session |
| `pseudocoder session rename <id> <name>` | rename a session |
| `pseudocoder session list-tmux` | list available tmux sessions |
| `pseudocoder session attach-tmux <name>` | attach to a tmux session |
| `pseudocoder session detach <id>` | detach from a tmux session |
| `pseudocoder devices list` | list paired devices |
| `pseudocoder devices revoke <id>` | revoke device access |
| `pseudocoder keep-awake enable-remote` | enable remote keep-awake policy |
| `pseudocoder keep-awake disable-remote` | disable remote keep-awake policy |
| `pseudocoder keep-awake status` | show keep-awake policy status |
| `pseudocoder start --enable-remote-keep-awake` | start with keep-awake enabled |
| `pseudocoder --version` | show version |

---

## Links

- [contributing](CONTRIBUTING.md)
- [privacy policy](PRIVACY.md)
- [changelog](CHANGELOG.md)
- [report an issue](https://github.com/diab-ma/pseudocoder-host/issues)
