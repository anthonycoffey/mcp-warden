---
id: ADR-001
title: Enumerate processes in-house per OS instead of using gopsutil
status: accepted
date: 2026-10-12
---

# ADR-001: In-house process enumeration

## Context

The orphan scanner (`scan`, `watch`) and `reap` need, for every process on the machine, its
PID, parent PID, executable name, command line, creation time, and resident memory. They
also need to kill a process only if it is still the same process.

[gopsutil](https://github.com/shirou/gopsutil) is the standard Go library for this and
covers far more (CPU, disks, network, sensors).

SPEC-001 sets a dependency budget of the standard library plus `golang.org/x/sys`. Anything
else needs an ADR.

## Decision

Implement `internal/procscan` in-house, with one file per OS:

- **Windows:** `CreateToolhelp32Snapshot` for the process list,
  `NtQueryInformationProcess(ProcessCommandLineInformation)` for command lines (needs only
  `PROCESS_QUERY_LIMITED_INFORMATION`, no PEB reads), `GetProcessTimes`, and
  `ProcessVmCounters`.
- **Linux:** `/proc/<pid>/{stat,cmdline,comm,exe}` and `btime` from `/proc/stat`.
- **macOS:** `sysctl kern.proc.all` and `kern.procargs2`.

## Consequences

**Good**

- The surface we need is small (~350 lines across three OSes) and fully understood.
- The kill path can check identity and kill **through the same handle** on Windows, which a
  general-purpose library API doesn't offer.
- No transitive dependencies. gopsutil pulls in several modules and, on some platforms and
  versions, needs cgo, which would break the "static binary, no cgo" promise.
- The OS-level code is part of what this project demonstrates.

**Bad**

- macOS RSS isn't available without cgo (`proc_pidinfo` lives in libproc). We report 0 and
  say so in the docs.
- Linux assumes `USER_HZ = 100`, which holds on every mainstream architecture but isn't read
  from `sysconf`.
- We own the maintenance. Mitigation: a smoke test per OS (`TestSnapshotFindsSelf`,
  `TestAliveAndKill`) runs in CI on all three.

## Alternatives considered

- **gopsutil:** rejected for the reasons above. Worth revisiting if we ever need CPU or
  per-process I/O stats.
- **Shelling out to `ps` / `wmic` / `Get-CimInstance`:** slow (PowerShell CIM queries take
  100s of ms), fragile to parse, and `wmic` is deprecated.
