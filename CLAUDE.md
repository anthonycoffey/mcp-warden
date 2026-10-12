# CLAUDE.md

Guidance for AI agents working in this repository.

## What this is

`mcp-warden`: a Go CLI that supervises local MCP servers (stdio proxy with restart and
session replay, process-tree ownership) and scans the machine for unsupervised and orphaned
MCP server processes. Start with `README.md`, then `docs/architecture.md`.

## Commands

```bash
go build ./...                  # build (no cgo)
go test ./...                   # unit + e2e (~15s)
make vet                        # go vet for windows, linux, darwin; per-OS files must compile everywhere
golangci-lint run ./...         # lint; also run with GOOS=linux and GOOS=darwin
go test -run=^$ -bench=. ./internal/proxy/
```

On Windows without a C toolchain `-race` is unavailable; CI runs it.

## Rules

- **Spec first.** New behavior starts as a spec in `docs/specs/active/`
  (`draft → ready → in-progress → review-pending → complete`); significant decisions get an
  ADR in `docs/adr/`. Tests first (RED → GREEN → REFACTOR).
- **Dependencies:** standard library + `golang.org/x/sys` only. Anything else needs an ADR.
- **Never write to stdout in `run`** except relayed or synthesized JSON-RPC. Use the logger
  (stderr + log file).
- **Proxy state is owned by the actor goroutine** (`internal/proxy`, ADR-002). Don't add
  mutexes around it; send events instead. Never block the actor on I/O.
- **Never kill by PID alone.** Use `procscan.Kill(pid, createTime)` / `KillTree` (ADR-004).
- **Keep policy pure.** Classification logic belongs in `internal/classify` (no OS calls),
  with table-driven tests built from hand-made snapshots.
- Per-OS code uses build tags/filename suffixes (`_windows.go`, `_linux.go`, `_darwin.go`,
  `//go:build unix`).
- e2e tests that kill must be scoped (`scan --kill --pid …`) so they never touch a
  developer's real processes.

## Commit format

`<type>: <description> [SPEC-XXX]`, with types `feat`, `fix`, `refactor`, `docs`, `test`,
`style`. For now changes are pushed straight to `main`; a PR workflow (branches `feature/`,
`fix/`, `refactor/`) starts once the codebase is established.
