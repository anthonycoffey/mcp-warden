# Architecture

mcp-warden is two tools sharing one binary and one state directory:

- **The supervisor** (`run`): a long-lived, per-server stdio proxy that owns the server's
  process tree.
- **The scanner** (`scan`, `watch`, `history`, plus `status` and `reap`): short-lived commands
  that inspect the machine and the supervisors' state files.

They never talk to each other directly. The state directory is the only interface between
them (see [ADR-003](adr/ADR-003-state-files-not-daemon.md)).

```mermaid
flowchart TB
    subgraph clients["MCP clients"]
        CD[Claude Desktop]
        CC[Claude Code]
        CU[Cursor / VS Code]
    end
    subgraph sup["supervisors (one per server)"]
        R1["mcp-warden run --name github"]
        R2["mcp-warden run --name figma"]
    end
    CD -- stdio --> R1
    CC -- stdio --> R2
    CU -- "stdio (unwrapped)" --> X["npx some-mcp-server"]
    R1 --> T1["Job Object: npx → node"]
    R2 --> T2["Job Object: uvx → python"]

    subgraph dir["state directory"]
        I[("instances/*.json")]
        L[("logs/*.log")]
        S[("scan.json")]
        H[("processes.jsonl")]
        CFG[("config.json")]
    end
    R1 & R2 -- "atomic writes, 1/s" --> I
    R1 & R2 --> L

    subgraph cmds["inspection commands"]
        ST[status]
        RP[reap]
        SC[scan / watch]
        HI[history]
    end
    I --> ST & RP & SC
    SC -- "snapshot of every process" --> OS[(OS process table)]
    SC --> S & H
    CFG --> SC
    H --> HI
    S --> ST
```

## Packages

```mermaid
flowchart BT
    main[cmd/mcp-warden] --> cli
    cli --> proxy & classify & history & state & procscan & logfile & metrics & restart
    proxy --> jsonrpc & restart & state & metrics & procgroup
    procgroup --> procscan
    classify --> procscan
    history --> classify & logfile & state
```

| Package | Responsibility | Talks to the OS? |
|---|---|---|
| `jsonrpc` | Newline framing with a size cap; envelope-only parse; error/notification builders | no |
| `restart` | Exponential backoff with jitter; sliding-window circuit breaker | no |
| `metrics` | Counters and a latency reservoir (p50/p95/max over the last 1024 requests) | no |
| `proxy` | The actor loop: relay, in-flight tracking, restart buffer, session replay | via `Child` |
| `procgroup` | Start a command so its whole tree can be killed (Job Object / process group) | yes |
| `procscan` | Snapshot every process; identity-checked kill; tree helpers | yes |
| `classify` | **Pure** policy: snapshot → classified trees | no |
| `history` | Diff scans → events; JSONL log; summaries | files |
| `state` | Instance schema, atomic JSON writes, directory layout | files |
| `logfile` | Size-rotating writer | files |
| `cli` | Flag parsing, commands, human and JSON output | — |

The OS-facing packages are kept thin. Everything with real decision logic (`classify`,
`restart`, `history`, the proxy's relay rules) is pure or sits behind an interface, which is
what makes the test suite fast and deterministic (see [testing.md](testing.md)).

## The supervisor

### Concurrency model

The proxy is an **actor**: one goroutine owns every piece of session state, and everything
that can block runs in another goroutine and talks to it through channels
([ADR-002](adr/ADR-002-actor-model-proxy.md)).

```mermaid
flowchart LR
    stdin([client stdin]) --> RC[readClient]
    RC -- "chan clientMsg" --> A

    subgraph gen["per child generation"]
        RS[readServer] 
        RE[readStderr]
        WT[wait]
    end
    RS -- "chan event{line}" --> A
    WT -- "chan event{exit}" --> A
    RE --> LOG[(log file + stderr)]

    A{{"actor loop<br/>Run()"}}
    A -- push --> QC[["lineQueue → client"]]
    A -- push --> QS[["lineQueue → child"]]
    QC -- Drain --> stdout([client stdout])
    QS -- Drain --> cstdin([child stdin])
    T((timers:<br/>restart · init · grace)) --> A
    SIG((ctx / signals)) --> A
```

- **Push never blocks.** `lineQueue` is an unbounded FIFO drained by its own writer
  goroutine. If the server stops reading its stdin, the actor keeps going, so server output
  keeps flowing to the client and the classic two-pipe deadlock can't happen.
- **One writer per stream.** Relayed messages and messages mcp-warden makes up itself (errors,
  `list_changed`) go through the same queue, so lines never interleave.
- **Generations.** Each child process is a numbered generation with its own reader, waiter
  and queue. Events carry their generation number, and the actor ignores anything from a
  generation that is no longer current.
- **Exit after output.** The `wait` goroutine sends the exit event only after the stdout
  reader has drained, so "server exited" is always ordered after the server's last message.
  After the direct child exits it also kills the rest of the tree, which closes any pipe
  handles grandchildren were holding.

### Lifecycle

```mermaid
stateDiagram-v2
    [*] --> running: spawn ok
    [*] --> spawn_error: command not found
    spawn_error --> [*]: exit 127 / 1
    running --> restarting: unexpected exit<br/>(in-flight requests failed)
    restarting --> restarting: backoff, spawn,<br/>replay initialize
    restarting --> running: replay answered
    restarting --> failed: breaker trips<br/>(> max-restarts in window)
    running --> stopped: stdin EOF / signal
    restarting --> stopped: stdin EOF / signal
    failed --> stopped: stdin EOF / signal
    stopped --> [*]
```

On shutdown the proxy closes the server's stdin (the MCP stdio shutdown signal), waits up
to `--grace`, then kills the tree. A clean exit deletes the instance's state file, so a state
file that is still there always means "this warden died", which is exactly what `reap` looks
for.

### State publishing

`run` publishes an `Instance` record (schema in [cli.md](cli.md#files)) once a second, and
within 250 ms of any phase change. It records the child's **OS creation time** rather than
mcp-warden's own clock, so `reap` can prove a PID still belongs to the same process before
killing it ([ADR-004](adr/ADR-004-creation-time-identity.md)).

## The scanner

```mermaid
sequenceDiagram
    participant U as scan / watch
    participant P as procscan
    participant St as state files
    participant C as classify
    participant H as history
    U->>P: Snapshot()
    P-->>U: []Process (pid, ppid, name, cmdline, createTime, rss)
    U->>St: live instances → managed child PIDs
    U->>C: Classify(snapshot, instances, policy)
    C-->>U: []Tree + unreadable count
    U->>H: Diff(previous scan, this scan)
    H-->>U: appeared / orphaned / disappeared
    U->>H: append processes.jsonl, write scan.json
    opt --kill or --auto-reap
        U->>P: fresh Snapshot(), KillTree(identity-checked)
        U->>H: append killed
    end
```

Classification rules, tree grouping, and the safety argument for killing are covered in
[process-ownership.md](process-ownership.md).

## Error handling philosophy

- **The protocol channel is sacred.** Nothing but JSON-RPC messages ever reaches the
  client's stdout. All diagnostics go to stderr (which clients capture into their own logs)
  and to the instance log file.
- **Never leave a request hanging.** Every request is either answered by the server or by
  mcp-warden: on crash (`server_restarted`), on a full buffer (`restart_buffer_full`), with
  the circuit open (`circuit_open`), or when a message is too large (`message_too_large`).
- **Degrade rather than fail.** A process the scanner can't read is counted as
  `unreadable`, not guessed at. If Job Object assignment fails, the supervisor falls back to
  killing the tree by walking it, and logs a warning.
