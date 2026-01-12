<p align="center">
  <img src="assets/github-banner.svg" alt="pseudocoder" width="600">
</p>

Supervise AI coding sessions from your phone. Watch terminal output, review code changes, and accept or reject diffs anytime, anywhere.

> **WARNING:**
> 
> THIS APP GRANTS FULL TERMINAL ACCESS TO PAIRED DEVICES. COMMANDS RUN WITH YOUR USER PRIVILEGES. **ONLY USE OVER TAILSCALE OR A TRUSTED LOCAL NETWORK. NEVER EXPOSE TO THE PUBLIC INTERNET.**

## prerequisites

- [Tailscale](https://tailscale.com/download) installed and running
- iphone with iOS 15+

## Installation

### macOS

```bash
brew install tailscale && brew services start tailscale  # Install Tailscale
sudo tailscale up                                         # Connect to Tailscale network
curl -sSL https://raw.githubusercontent.com/diab-ma/pseudocoder-host/main/install.sh | bash  # Install pseudocoder
```

### Linux

```bash
curl -fsSL https://tailscale.com/install.sh | sh  # Install Tailscale
sudo tailscale up                                  # Connect to Tailscale network
curl -sSL https://raw.githubusercontent.com/diab-ma/pseudocoder-host/main/install.sh | bash  # Install pseudocoder
```

### Windows (WSL2)

Run inside WSL:

```bash
curl -fsSL https://tailscale.com/install.sh | sh  # Install Tailscale
sudo tailscaled &                                  # Start Tailscale daemon
sudo tailscale up                                  # Connect to Tailscale network
curl -sSL https://raw.githubusercontent.com/diab-ma/pseudocoder-host/main/install.sh | bash  # Install pseudocoder
```

Also install Tailscale on your iphone and sign in with the same account.

---

## Quick Start

1. Get the mobile app (check your email for TestFlight invite)
2. Note your Tailscale Ip: `tailscale ip -4` (starts with `100.`)
3. Start the host:
   ```bash
   cd /path/to/your/project    # Navigate to your project
   pseudocoder start --pair --qr  # Start host and show pairing QR
   ```
4. Scan the QR code from the app and tap **Trust**

---

## Usage

<h3 align="center">Sessions</h3>

Create multiple terminal sessions, easily switch via the session pill, and attach to an existing tmux session to continue right where you left off.

<p align="center">
  <img src="assets/screenshot-new-session.png" alt="New Session" width="300">
</p>

<h3 align="center">Terminal</h3>

Real-time terminal output streaming. Send terminal commands, monitor CLI tools as they work, and accept changes from wherever you are.

<p align="center">
  <img src="assets/screenshot-terminal.png" alt="Terminal" width="300">
</p>

<h3 align="center">Review Cards</h3>

Commit panel in Review tab allows to frictonless way to enter message, commit, and push. Track all your accepted/rejected changes and easily undo.

- **Accept** stages changes (`git add`)
- **Reject** restores original (`git restore`)
- **Undo** reverts decision (`git restore --staged` or `git checkout`)
- **Commit** creates a commit (`git commit`)
- **Push** pushes to remote (`git push`)

<p align="center">
  <img src="assets/screenshot-review.png" alt="Review and Commit" width="300">
</p>

---

## Security

Use Tailscale or trusted LAN only. i recommend never exposing to public internet. All connections use TLS 1.2+. paired devices have full terminal access. Only pair devices you control.

Manage devices:
```bash
pseudocoder devices list          # View paired devices
pseudocoder devices revoke <id>   # Remove access
```

---

## Troubleshooting

| problem | Solution |
|---------|----------|
| Connection refused | Use Tailscale Ip (`100.x.x.x`), check `tailscale status` |
| Certificate errors | Tap **Trust** on first connection, or re-pair |
| pairing code expired | Run `pseudocoder pair --qr` (codes last 5 min) |
| QR won't scan | Enter manually: host Ip, port `7070`, 6-digit code |
| macOS Gatekeeper | `xattr -d com.apple.quarantine /usr/local/bin/pseudocoder` |

---

## Command Reference

| Command | Description |
|---------|-------------|
| `pseudocoder start --pair --qr` | Start host with pairing QR |
| `pseudocoder pair --qr` | Generate new pairing code |
| `pseudocoder --version` | Show version |
| `pseudocoder devices list` | List paired devices |
| `pseudocoder devices revoke <id>` | Revoke device access |

---

## Links

- [Contributing](CONTRIBUTING.md)
- [privacy policy](PRIVACY.md)
- [Changelog](CHANGELOG.md)
- [Report an issue](https://github.com/diab-ma/pseudocoder-host/issues)
