---
id: ADR-004
title: A process is identified by (PID, creation time), never PID alone
status: accepted
date: 2026-10-12
---

# ADR-004: Creation-time process identity

## Context

mcp-warden kills processes it didn't just start: leftovers of a dead warden (`reap`) and
orphans found by a scan (`scan --kill`, `watch --auto-reap`). Between observing a PID and
acting on it, the process can exit and its PID can be reused by an unrelated program.
Windows reuses PIDs aggressively, and the scan-to-kill gap can be minutes when the user is
reading a confirmation prompt.

PID reuse also breaks parent links: Windows never re-parents orphans, so an orphan's PPID
may now belong to a newer, unrelated process.

## Decision

- Every recorded process reference stores the OS creation time next to the PID
  (`wardenCreatedAt`, `childStartedAt`, `Process.CreateTime`, and tree keys
  `<pid>@<ms>`).
- `procscan.Kill(pid, created)` re-reads the creation time immediately before killing and
  returns `ErrIdentityMismatch` if it differs. On Windows the check and `TerminateProcess`
  use the same handle, so there's no race at all.
- A parent link is trusted only if the parent was created no later than the child.
- Times are compared with a 10ms tolerance, because Linux derives them from clock ticks and
  they round-trip through JSON.

## Consequences

**Good**

- A reused PID can't be killed by mistake. This is the property that makes it acceptable
  for an orphan cleaner to exist at all.
- Orphans on Windows are detected correctly despite stale PPIDs.

**Bad**

- On Linux and macOS there is a microseconds-wide window between check and kill. A `pidfd`
  (Linux 5.3+) would close it and is a candidate follow-up.
- Every kill costs an extra syscall, which is negligible.
