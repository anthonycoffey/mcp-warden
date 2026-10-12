# Changelog

All notable changes are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and versions follow
[Semantic Versioning](https://semver.org/).

## [Unreleased]

### Added

- `run`: transparent stdio proxy for MCP servers with process-tree ownership (Windows Job
  Objects, Unix process groups), restart with exponential backoff, circuit breaker,
  in-flight request failure, MCP handshake replay, and `list_changed` notifications after a
  restart. ([SPEC-001](docs/specs/active/SPEC-001-mcp-warden-prototype.md))
- `status`, `logs`, `reap` for supervised instances.
- `scan`, `watch`, `history`: machine-wide MCP process classification (managed, unmanaged,
  orphaned, detached), identity-checked orphan cleanup, and leak history.
- In-house process enumeration for Windows, Linux and macOS
  ([ADR-001](docs/adr/ADR-001-in-house-process-enumeration.md)).
