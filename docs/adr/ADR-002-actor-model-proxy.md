---
id: ADR-002
title: Single-goroutine actor owns all proxy state
status: accepted
date: 2026-10-12
---

# ADR-002: Actor-model proxy

## Context

The proxy juggles a lot of state that changes together: the current child generation,
in-flight requests, the restart buffer, the captured handshake, server→client request
ownership, the circuit breaker, and three timers. Events arrive concurrently from the
client, the child's stdout, the child's exit, timers, and signals.

The obvious design, two pump goroutines (`client→server`, `server→client`) sharing state
under a mutex, has two problems:

1. **Lock discipline across restarts.** Swapping the child, failing in-flight requests and
   flushing the buffer have to be atomic with respect to both pumps. That means coarse
   locks held across I/O, or subtle fine-grained ones.
2. **The two-pipe deadlock.** If a pump writes to the child's stdin while holding the lock,
   and the child is blocked writing to its stdout because nobody is reading, both sides
   stall.

## Decision

One goroutine (the **actor**, `proxy.Run`) owns every field of the session state. Everything
else is a producer that sends to it over channels:

- a client reader, plus a stdout reader, stderr reader and exit waiter per generation;
- timers, as `time.Timer` channels selected on directly;
- `ctx.Done()` for signals.

All writes go through **unbounded `lineQueue`s** drained by dedicated writer goroutines, so
the actor never blocks on I/O. Events carry a generation number, and stale ones are ignored.

## Consequences

**Good**

- No locks around session state, so the restart logic reads as straight-line code.
- The deadlock can't happen: the actor always gets back to its `select`, so server output
  keeps draining.
- One writer per output stream, so relayed messages and messages mcp-warden makes up itself
  can never interleave inside a line.
- Ordering is easy to reason about. For example, the exit event is sent only after the
  stdout reader has drained, so "server exited" always comes after its last message.
- Deterministic tests: the in-memory `Child` makes every interleaving the tests care about
  reproducible.

**Bad**

- `lineQueue` is unbounded. In practice memory is bounded by the restart buffer (256) and
  the client's own request rate, but a server that floods stdout while the client stops
  reading would grow it. Accepted for the prototype; a high-water mark is a follow-up.
- Producer goroutines blocked on reads (`stdin`) can't be cancelled in Go. They're released
  when the process exits, which is the only time `Run` returns in production.
