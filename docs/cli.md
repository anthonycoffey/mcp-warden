# CLI reference

```
mcp-warden <command> [flags]
```

Every command accepts `-h`. Commands that read or write state accept `--state-dir DIR`
(default: the platform state directory, or `$MCP_WARDEN_STATE_DIR`).

**Exit codes:** `0` success · `1` runtime error · `2` usage error · `127` (`run` only) the
server command was not found.

---

## run

Run an MCP server under supervision. This is the command that goes in a client config.

```
mcp-warden run --name <name> [flags] -- <command> [args...]
```

| Flag | Default | |
|---|---|---|
| `--name` | *(required)* | Instance name: letters, digits, `-`, `_`, `.`; at most 64 characters. If a live instance already uses it, `-<pid>` is appended. |
| `--grace` | `3s` | How long the server may take to exit after its stdin is closed. |
| `--max-restarts` | `5` | Crashes allowed within `--restart-window` before giving up. |
| `--restart-window` | `1m` | Sliding window for `--max-restarts`. |
| `--backoff-base` | `250ms` | First restart delay. |
| `--backoff-max` | `30s` | Maximum restart delay. |
| `--buffer` | `256` | Client messages held while the server restarts. |
| `--init-timeout` | `10s` | Time for a restarted server to answer the replayed `initialize`. |
| `--max-message` | `16777216` | Maximum message size in bytes. |
| `-v` | off | Debug logging to stderr (the log file always gets debug). |

Behavior is specified in [protocol.md](protocol.md).

## status

```
mcp-warden status [--json]
```

Lists every instance with its state (`running`, `restarting`, `failed`, or **`stale`** if its
mcp-warden process is gone), PIDs, uptime, restarts, request and error counts, and p50/p95
latency. Below the table it summarizes the most recent machine scan and points at `reap` if
any instance is stale.

`--json` prints `{ "instances": [ Instance + "stale" ], "scan": { …counts } }`.

## logs

```
mcp-warden logs <name> [-f] [-n 100]
```

Prints the last `-n` lines (0 for all) of the instance's log. `-f` keeps following it,
across rotations too. The log is JSON Lines from `log/slog`; the server's stderr shows up
as `"msg":"server stderr"` records.

## reap

```
mcp-warden reap [--dry-run]
```

For every state file whose mcp-warden process is dead: if the recorded child is still
running *and its creation time matches*, kill it and all of its descendants. On Linux and
macOS, also kill anything still in the child's **process group** that was created after the
child. That catches grandchildren whose parent already exited, which are no longer
descendants of anything. Then delete the state file. If the child's PID now belongs to a
different process, nothing is touched. `--dry-run` reports what would happen and changes
nothing.

## scan

```
mcp-warden scan [--json] [--all] [--kill [--yes] [--pid P]...] [--match RE]... [--exclude RE]...
```

| Flag | |
|---|---|
| `--json` | Print the full classified snapshot (`{"time":…, "result":{"trees":[…], "unreadable":N}}`). |
| `--all` | Include `managed` trees in the table (hidden by default). |
| `--kill` | Kill `orphaned` trees. Asks for confirmation on a terminal; refuses on non-terminals unless `--yes`. |
| `--yes` | Skip the confirmation. |
| `--pid P` | With `--kill`: only the orphaned tree rooted at PID `P`. Repeatable. |
| `--match RE` | Extra regexp marking a command line as an MCP server. Repeatable. |
| `--exclude RE` | Regexp of command lines to ignore. Repeatable. |

Every scan updates `scan.json` and appends change events to `processes.jsonl`, so occasional
`scan`s build history even without `watch`. The classes and the kill safety rules are
explained in [process-ownership.md](process-ownership.md).

## watch

```
mcp-warden watch [--interval 30s] [--auto-reap] [--min-age 5m] [--match RE]... [--exclude RE]...
```

Scans every `--interval` (minimum 1s) and prints one line per change event:

```
14:02:10  appeared    unmanaged pid 60616   6 proc   178 MB  cmd.exe /d /s /c "npx chrome-devtools-mcp@latest"
14:31:40  orphaned    orphaned  pid 60616   6 proc   178 MB  cmd.exe /d /s /c "npx chrome-devtools-mcp@latest"
14:36:40  killed      orphaned  pid 60616   6 proc   178 MB  cmd.exe /d /s /c "npx chrome-devtools-mcp@latest"
```

With `--auto-reap`, a tree that has stayed orphaned for `--min-age` is killed (same safety
checks as `scan --kill`). Off by default.

To keep `watch` running in the background, register it with your OS scheduler. For example,
on Windows:

```powershell
schtasks /Create /SC ONLOGON /TN mcp-warden-watch /TR "mcp-warden watch --interval 60s"
```

## history

```
mcp-warden history [--since 24h] [--top 10] [--json]
```

`--since` accepts Go durations plus days (`90m`, `24h`, `7d`). The output shows orphans per
day, the most frequently orphaned commands (with kill counts and peak memory), and the
peak total memory held by simultaneous orphans.

```
Since 2026-10-05 09:00: 14 orphaned trees, 9 killed, peak orphaned memory 1.2 GB at 2026-10-09 17:42

Orphans per day:
  2026-10-07    5  █████
  2026-10-09    9  █████████

Most frequently orphaned:
  ORPHANS  KILLED  PEAK MEM  COMMAND
  8        6       212 MB    cmd.exe /d /s /c "npx -y @heroku/mcp-server"
  6        3       178 MB    cmd.exe /d /s /c "npx chrome-devtools-mcp@latest"
```

## version

```
mcp-warden version
```

---

## Files

Everything lives under the state directory:

```
<state-dir>/
├── instances/<name>.json   one per running `run` (deleted on clean exit)
├── logs/<name>.log         slog JSON lines; rotated at 10 MiB, 3 backups (.1 … .3)
├── scan.json               latest classified snapshot
├── processes.jsonl         scan change events; rotated like logs
└── config.json             optional scanner policy (you create this)
```

### `instances/<name>.json` (schema 1)

```json
{
  "schema": 1,
  "name": "github",
  "command": ["npx", "-y", "@modelcontextprotocol/server-github"],
  "wardenPid": 41220,
  "wardenCreatedAt": "2026-10-12T15:00:00.1234567Z",
  "childPid": 41236,
  "childStartedAt": "2026-10-12T15:04:05.4567891Z",
  "state": "running",
  "startedAt": "2026-10-12T15:00:00Z",
  "lastRestartAt": "2026-10-12T15:04:05Z",
  "restarts": 1,
  "lastExit": "exit status 1",
  "counts": { "requests": 42, "notifications": 9, "errors": 1 },
  "latencyMs": { "p50": 38, "p95": 410, "max": 1290 },
  "updatedAt": "2026-10-12T15:06:11Z"
}
```

`wardenCreatedAt` and `childStartedAt` are **OS process creation times**, used to tell a
live process from a reused PID.

### `processes.jsonl`

```json
{"time":"2026-10-12T14:31:40Z","type":"orphaned","key":"60616@1760279530123","pid":60616,"class":"orphaned","command":"cmd.exe /d /s /c \"npx chrome-devtools-mcp@latest\"","procs":6,"rss":186646528}
```

`type` is one of `appeared`, `orphaned`, `disappeared`, `killed`. `key` is
`<root pid>@<creation time in ms>`, which identifies a tree across scans.

### `config.json`

```json
{
  "match":   ["my-internal-tool --stdio"],
  "exclude": ["^vim "],
  "clients": [{ "label": "Acme IDE", "name": "^acme(\\.exe)?$", "cmdline": "" }]
}
```

All three lists **extend** the built-in defaults. `name` is matched against the executable
name, and `cmdline` against the full command line.
