---
id: ADR-003
title: Per-instance state files instead of a daemon or socket
status: accepted
date: 2026-10-12
---

# ADR-003: State files, not a daemon

## Context

`status`, `reap` and `scan` need to know about every running `mcp-warden run`. Options:

1. A background daemon that supervisors register with over a local socket or named pipe.
2. Each supervisor serves its own local HTTP or IPC endpoint.
3. Each supervisor writes a small JSON file to a shared directory.

## Decision

Option 3: each instance atomically replaces `<state-dir>/instances/<name>.json` (write a
temp file in the same directory, then `rename`) at most once per second, plus shortly after
phase changes. It deletes the file on clean exit.

## Consequences

**Good**

- **No daemon to install, start or keep alive.** Installing mcp-warden is copying one binary.
- **No ports or pipes.** Nothing to firewall, nothing that collides, nothing to secure.
- **Crash semantics come for free.** A state file whose `wardenPid` is dead (checked with
  creation time, [ADR-004](ADR-004-creation-time-identity.md)) *is* the record of a crash,
  and it's exactly what `reap` needs.
- Humans can inspect it (`cat`), and other tools can read it.

**Bad**

- Data is up to ~1s stale. That's fine for a status view.
- On Windows, a reader can briefly hit a sharing violation during a rename. `ReadFile` and
  the rename both retry a few times.
- No push notifications to a UI. A future dashboard would poll the directory, which is
  cheap at this scale (a handful of files).
