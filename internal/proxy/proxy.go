// Package proxy is the heart of `mcp-warden run`: a transparent stdio relay
// between an MCP client and a supervised MCP server that survives server
// crashes without the client noticing more than one failed call.
//
// # Concurrency model
//
// All session state is owned by one goroutine, the actor loop in Run.
// Everything that blocks lives elsewhere:
//
//   - a client reader goroutine turns stdin into messages on a channel;
//   - per child generation, a stdout reader, a stderr reader, and a waiter
//     goroutine turn the child's output and exit into events;
//   - two lineQueues (to the client, to the current child) are drained by
//     their own writer goroutines, so the actor never blocks on a write.
//
// Because of that, there are no locks around session state, and a child that
// stops reading its stdin cannot stall the relay in the other direction.
package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/jsonrpc"
	"github.com/anthonycoffey/mcp-warden/internal/metrics"
	"github.com/anthonycoffey/mcp-warden/internal/restart"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

// Reserved prefix for ids mcp-warden injects. MCP clients use numeric or
// UUID ids, so a collision is not a practical concern.
const reservedIDPrefix = "mcp-warden:"

// Reasons reported in error.data.reason of synthesized responses.
const (
	ReasonServerRestarted = "server_restarted"
	ReasonBufferFull      = "restart_buffer_full"
	ReasonCircuitOpen     = "circuit_open"
	ReasonTooLarge        = "message_too_large"
)

// Config configures a proxy run. Zero values select SPEC-001 defaults.
type Config struct {
	Spawn Spawner
	In    io.Reader // from the client
	Out   io.Writer // to the client; nothing but protocol messages is ever written here

	Log         *slog.Logger
	Metrics     *metrics.Metrics
	ChildStderr func(line string) // receives each stderr line of the server
	OnStatus    func(s Status)    // called from the actor on every status change; must not block

	Backoff       restart.Backoff
	MaxRestarts   int           // default 5
	RestartWindow time.Duration // default 60s
	BufferSize    int           // default 256
	MaxMessage    int           // default 16 MiB
	InitTimeout   time.Duration // default 10s
	Grace         time.Duration // default 3s
}

func (c *Config) defaults() {
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
	if c.Metrics == nil {
		c.Metrics = metrics.New()
	}
	if c.Backoff.Base == 0 {
		c.Backoff = restart.DefaultBackoff()
	}
	if c.MaxRestarts == 0 {
		c.MaxRestarts = 5
	}
	if c.RestartWindow == 0 {
		c.RestartWindow = time.Minute
	}
	if c.BufferSize == 0 {
		c.BufferSize = 256
	}
	if c.MaxMessage == 0 {
		c.MaxMessage = jsonrpc.DefaultMaxMessage
	}
	if c.InitTimeout == 0 {
		c.InitTimeout = 10 * time.Second
	}
	if c.Grace == 0 {
		c.Grace = 3 * time.Second
	}
}

// Status is the supervisor-level view of the proxy, published via OnStatus.
type Status struct {
	Phase          state.Phase
	ChildPID       int
	ChildStartedAt time.Time
	Restarts       int
	LastRestartAt  time.Time
	LastExit       string
}

// SpawnError reports that the server could not be started at all. It is not
// retried: a missing binary will not appear by waiting.
type SpawnError struct{ Err error }

func (e *SpawnError) Error() string { return "start MCP server: " + e.Err.Error() }
func (e *SpawnError) Unwrap() error { return e.Err }

type inflight struct {
	id     json.RawMessage
	method string
	start  time.Time
}

// event is something that happened to a child generation.
type event struct {
	gen  int
	line []byte // a stdout message, or
	err  error  // a framing error, or
	exit *exitInfo
}

type exitInfo struct{ err error }

type clientMsg struct {
	line []byte
	err  error
}

// proxy is the actor's state. Every field is owned by the goroutine running
// Run; other goroutines communicate with it only through channels and
// lineQueues.
type proxy struct {
	cfg Config
	log *slog.Logger

	toClient *lineQueue
	events   chan event
	quit     chan struct{}

	// Current generation. child is nil while restarting or failed.
	gen          int
	child        Child
	toChild      *lineQueue
	childStarted time.Time
	killReason   string

	phase    state.Phase
	replayID string // jsonrpc key of the in-flight replayed initialize, "" if none
	stopping bool

	// MCP session captured from the client, replayed into new generations.
	initParams        json.RawMessage
	initKey           string
	negotiatedVersion string // protocolVersion the original server agreed to
	initializedMsg    []byte
	sessionUp         bool

	inflight   map[string]inflight
	serverReqs map[string]int // server→client request key → generation
	buffer     [][]byte

	breaker       restart.Breaker
	restarts      int
	lastRestartAt time.Time
	lastExit      string

	restartTimer *time.Timer
	initTimer    *time.Timer
	graceTimer   *time.Timer
}

// Run relays between cfg.In/cfg.Out and a supervised server until the client
// closes its input or ctx is cancelled. It returns nil on a clean shutdown and
// a *SpawnError if the server could not be started the first time.
func Run(ctx context.Context, cfg Config) error {
	cfg.defaults()
	p := &proxy{
		cfg:        cfg,
		log:        cfg.Log,
		toClient:   newLineQueue(),
		events:     make(chan event, 64),
		quit:       make(chan struct{}),
		inflight:   map[string]inflight{},
		serverReqs: map[string]int{},
		breaker:    restart.Breaker{Max: cfg.MaxRestarts, Window: cfg.RestartWindow},
	}
	defer close(p.quit)
	go p.toClient.Drain(cfg.Out, nil)

	if err := p.spawn(); err != nil {
		p.toClient.Close()
		return &SpawnError{Err: err}
	}

	clientIn := make(chan clientMsg, 64)
	go p.readClient(clientIn)

	ctxDone := ctx.Done()
	for {
		select {
		case m, ok := <-clientIn:
			if !ok {
				clientIn = nil
				p.beginStop("client closed stdin")
				break
			}
			if m.err != nil {
				p.handleClientFramingError(m.err)
				break
			}
			p.handleClient(m.line)
		case ev := <-p.events:
			p.handleEvent(ev)
		case <-timerC(p.restartTimer):
			p.restartTimer = nil
			p.respawn()
		case <-timerC(p.initTimer):
			p.initTimer = nil
			p.log.Error("server did not answer the replayed initialize in time", "timeout", p.cfg.InitTimeout)
			p.killChild("initialize replay timed out")
		case <-timerC(p.graceTimer):
			p.graceTimer = nil
			p.log.Warn("server did not exit within grace period; killing process tree", "grace", p.cfg.Grace)
			p.killChild("killed after grace period")
		case <-ctxDone:
			ctxDone = nil
			p.beginStop("signal")
		}
		if p.stopping && p.child == nil {
			break
		}
	}

	p.setPhase(state.Stopped)
	p.toClient.Close()
	select {
	case <-p.toClient.Done():
	case <-time.After(2 * time.Second):
	}
	return nil
}

func timerC(t *time.Timer) <-chan time.Time {
	if t == nil {
		return nil
	}
	return t.C
}

func stopTimer(t **time.Timer) {
	if *t != nil {
		(*t).Stop()
		*t = nil
	}
}
