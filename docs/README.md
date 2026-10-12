# mcp-warden documentation

| Document | Read it when you want to… |
|---|---|
| [architecture.md](architecture.md) | understand how the pieces fit: packages, goroutines, data flow, state files |
| [protocol.md](protocol.md) | know exactly what happens to JSON-RPC traffic: relay rules, restart and session replay, synthesized errors |
| [process-ownership.md](process-ownership.md) | understand process-tree ownership, orphan detection, PID-reuse safety, and the per-OS details |
| [cli.md](cli.md) | look up a command, flag, exit code, or file in the state directory |
| [testing.md](testing.md) | run, extend, or understand the test suite |
| [adr/](adr/) | see *why* a decision was made (Architecture Decision Records) |
| [specs/](specs/) | read the feature specs the code was built from |

## Spec lifecycle

Work starts as a spec in `specs/active/` and moves through:

```
draft → ready → in-progress → review-pending → complete
```

Code is written only against a `ready` spec, test-first. Once a spec ships it moves to
`specs/archive/`. Decisions with lasting consequences get an ADR.

| Spec | Status |
|---|---|
| [SPEC-001: mcp-warden prototype](specs/active/SPEC-001-mcp-warden-prototype.md) | review-pending |

| ADR | Decision |
|---|---|
| [ADR-001](adr/ADR-001-in-house-process-enumeration.md) | Enumerate processes in-house per OS instead of using gopsutil |
| [ADR-002](adr/ADR-002-actor-model-proxy.md) | Single-goroutine actor owns all proxy state |
| [ADR-003](adr/ADR-003-state-files-not-daemon.md) | Per-instance state files instead of a daemon or socket |
| [ADR-004](adr/ADR-004-creation-time-identity.md) | A process is identified by (PID, creation time), never PID alone |
