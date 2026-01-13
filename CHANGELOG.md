# Changelog

All notable changes to pseudocoder will be documented in this file.

## [Unreleased]

## [0.1.0-alpha.1] - 2025-01-11

Initial alpha release.

### Features

- **Terminal streaming**: Real-time pTY output to mobile over WebSocket
- **Review cards**: Diff-based code review with accept/reject actions
- **per-chunk decisions**: Fine-grained control over individual hunks
- **Commit & push**: Stage changes and push from mobile
- **Multi-session**: Create and switch between terminal sessions
- **tmux integration**: Attach to existing tmux sessions
- **QR pairing**: Scan to connect with automatic TLS trust
- **Device management**: List and revoke paired devices

### Security

- TLS 1.2+ encryption (self-signed certificates)
- Token-based authentication with bcrypt hashing
- Rate-limited pairing (5 attempts per minute)
- Loopback-only admin endpoints

### platforms

- macOS (Apple Silicon, Intel)
- Linux (x86_64)
- Windows via WSL2
