# Changelog

All notable changes to pseudocoder will be documented in this file.

## [Unreleased]

## [0.2.0-beta] - 2026-03-XX

### Features

- **File operations**: list, read, write (version-conflict detection), create, delete, and watch (external change polling) for repository files
- **Git operations**: paginated history, branch list/create/switch (dirty-tree guardrails), fetch, pull (fast-forward-only), request-scoped commit/push correlation
- **Keep-awake system**: host-authoritative state machine (OFF/PENDING/ON/DEGRADED), multi-client lease arbitration, CLI commands (`keep-awake enable-remote/disable-remote/status`), `start --enable-remote-keep-awake` flag, battery policy enforcement (macOS caffeinate), SQLite audit persistence
- **Diagnostics**: `pseudocoder doctor` with preflight checks (TLS, network, pairing IPC, host readiness), `--json` output
- **Semantic analysis**: tree-sitter enrichment for diff cards, sensitive path detection
- **Rollout controls**: `pseudocoder flags` CLI (list/enable/disable/promote/rollout-stage/kill-switch)

### Improvements

- **Pairing redesign**: staged QR + manual flows, zero-trust IPC isolation, fail-closed fallback, 2-min TTL, host-global cooldown, replay/reuse rejection
- **Error taxonomy**: standardized error codes with TLS path propagation
- **Commit readiness**: preflight gating with advisory override
- **Risk-first review queue**: triage metadata, urgency timer
- **Onboarding recovery**: actionable recovery UX
- **Session management**: internal sessions hidden, read-only recovery

### Security

- Keep-awake policy mutations restricted to loopback-only API with Bearer-token auth
- Zero-trust pairing with IPC isolation
- File operations scoped to the supervised repository directory

## [0.1.0-beta] - 2026-01-28

### Features

- **IPC pairing**: socket-based pairing flow for secure local code exchange
- **Chunk grouping**: proximity-based algorithm groups related diff hunks into cohesive review sections
- **File decomposition**: code modularization and package restructuring
- **Atomic decisions**: transaction-based accept/reject with validation and rollback
- **Stable chunk numbering**: content-hash-based display IDs for deterministic chunk ordering

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
