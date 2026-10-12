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
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/jsonrpc"
	"github.com/anthonycoffey/mcp-warden/internal/metrics"
	"github.com/anthonycoffey/mcp-warden/internal/state"
	"github.com/anthonycoffey/mcp-warden/internal/supervisor"
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

	Backoff       supervisor.Backoff
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
		c.Backoff = supervisor.DefaultBackoff()
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
	initParams     json.RawMessage
	initKey        string
	initVersion    string
	initializedMsg []byte
	sessionUp      bool

	inflight   map[string]inflight
	serverReqs map[string]int // server→client request key → generation
	buffer     [][]byte

	breaker       supervisor.Breaker
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
		breaker:    supervisor.Breaker{Max: cfg.MaxRestarts, Window: cfg.RestartWindow},
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

// ---------------------------------------------------------------------------
// Child lifecycle

func (p *proxy) spawn() error {
	child, err := p.cfg.Spawn()
	if err != nil {
		return err
	}
	p.gen++
	gen := p.gen
	p.child = child
	p.childStarted = time.Now()
	p.killReason = ""
	p.toChild = newLineQueue()
	go p.toChild.Drain(child.Stdin(), child.Stdin())

	stdoutDone := make(chan struct{})
	go p.readServer(gen, child.Stdout(), stdoutDone)
	go p.readStderr(child.Stderr())
	go p.wait(gen, child, stdoutDone)

	p.log.Info("server started", "pid", child.Pid(), "generation", gen)
	if d, ok := child.(interface{ Degraded() error }); ok && d.Degraded() != nil {
		p.log.Warn("process-tree ownership degraded; falling back to tree walking", "err", d.Degraded())
	}

	if p.sessionUp {
		p.startReplay()
	} else {
		p.setPhase(state.Running)
		p.flushBuffer()
	}
	return nil
}

// respawn runs when the backoff timer fires.
func (p *proxy) respawn() {
	p.restarts++
	p.lastRestartAt = time.Now()
	if err := p.spawn(); err != nil {
		p.log.Error("restart failed", "err", err)
		p.afterExit(err.Error())
	}
}

func (p *proxy) killChild(reason string) {
	if p.child == nil {
		return
	}
	p.killReason = reason
	if err := p.child.Kill(); err != nil {
		p.log.Error("kill server process tree", "err", err)
	}
}

// wait reports a generation's exit once its stdout has been fully read, so
// the exit event is always ordered after the last message.
func (p *proxy) wait(gen int, child Child, stdoutDone <-chan struct{}) {
	err := child.Wait()
	// Reap whatever the server left behind (e.g. npx's node grandchild),
	// which also closes any pipe handles they hold.
	_ = child.Kill()
	select {
	case <-stdoutDone:
	case <-time.After(2 * time.Second):
	}
	p.send(event{gen: gen, exit: &exitInfo{err: err}})
}

func (p *proxy) send(ev event) {
	select {
	case p.events <- ev:
	case <-p.quit:
	}
}

func (p *proxy) readServer(gen int, r io.Reader, done chan<- struct{}) {
	defer close(done)
	rd := jsonrpc.NewReader(r, p.cfg.MaxMessage)
	for {
		line, err := rd.Next()
		if errors.Is(err, jsonrpc.ErrTooLong) {
			p.send(event{gen: gen, err: err})
			continue
		}
		if err != nil {
			return
		}
		p.send(event{gen: gen, line: line})
	}
}

func (p *proxy) readStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if p.cfg.ChildStderr != nil {
			p.cfg.ChildStderr(sc.Text())
		}
	}
}

func (p *proxy) readClient(out chan<- clientMsg) {
	defer close(out)
	rd := jsonrpc.NewReader(p.cfg.In, p.cfg.MaxMessage)
	for {
		line, err := rd.Next()
		if errors.Is(err, jsonrpc.ErrTooLong) {
			out <- clientMsg{err: err}
			continue
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				p.log.Error("read from client", "err", err)
			}
			return
		}
		out <- clientMsg{line: line}
	}
}

func (p *proxy) handleEvent(ev event) {
	if ev.gen != p.gen {
		return // a previous generation's leftovers
	}
	switch {
	case ev.exit != nil:
		p.handleExit(ev.exit.err)
	case ev.err != nil:
		p.handleServerFramingError(ev.err)
	default:
		p.handleServer(ev.line)
	}
}

func (p *proxy) handleExit(err error) {
	reason := "exit status 0"
	if err != nil {
		reason = err.Error()
	}
	if p.killReason != "" {
		reason = p.killReason
	}
	if cerr := p.child.Close(); cerr != nil {
		p.log.Debug("close child", "err", cerr)
	}
	p.child = nil
	p.toChild.Close()
	p.replayID = ""
	stopTimer(&p.initTimer)
	stopTimer(&p.graceTimer)

	if p.stopping {
		p.log.Info("server exited", "reason", reason)
		return
	}
	p.log.Warn("server exited unexpectedly", "reason", reason, "uptime", time.Since(p.childStarted).Round(time.Millisecond))
	p.afterExit(reason)
}

// afterExit fails in-flight work and decides between restarting and giving up.
func (p *proxy) afterExit(reason string) {
	p.lastExit = reason
	p.failInflight()

	now := time.Now()
	if p.breaker.Record(now) {
		p.log.Error("server is crash-looping; giving up",
			"max_restarts", p.cfg.MaxRestarts, "window", p.cfg.RestartWindow)
		p.setPhase(state.Failed)
		for _, line := range p.buffer {
			p.rejectIfRequest(jsonrpc.Parse(line), "MCP server failed and will not be restarted", ReasonCircuitOpen)
		}
		p.buffer = nil
		return
	}
	delay := p.cfg.Backoff.Delay(p.breaker.Recent(now) - 1)
	p.log.Info("restarting server", "in", delay.Round(time.Millisecond))
	p.setPhase(state.Restarting)
	p.restartTimer = time.NewTimer(delay)
}

func (p *proxy) failInflight() {
	for key, f := range p.inflight {
		p.cfg.Metrics.Error()
		p.toClient.Push(jsonrpc.ErrorResponse(f.id, jsonrpc.CodeInternalError,
			"MCP server restarted while handling "+f.method,
			map[string]string{"reason": ReasonServerRestarted}))
		delete(p.inflight, key)
	}
}

func (p *proxy) beginStop(why string) {
	if p.stopping {
		return
	}
	p.stopping = true
	p.log.Info("shutting down", "reason", why)
	stopTimer(&p.restartTimer)
	stopTimer(&p.initTimer)
	if p.child == nil {
		return
	}
	// Closing stdin is the MCP stdio shutdown signal; the grace timer
	// escalates to a tree kill if the server ignores it.
	p.toChild.Close()
	p.graceTimer = time.NewTimer(p.cfg.Grace)
}

// ---------------------------------------------------------------------------
// Session replay

func (p *proxy) startReplay() {
	p.setPhase(state.Restarting) // still restarting until the session is restored
	p.replayID = jsonrpc.IDKey(mustJSON(fmt.Sprintf("%sinit:%d", reservedIDPrefix, p.gen)))
	p.toChild.Push(jsonrpc.NewRequest(fmt.Sprintf("%sinit:%d", reservedIDPrefix, p.gen), "initialize", p.initParams))
	p.initTimer = time.NewTimer(p.cfg.InitTimeout)
	p.log.Info("replaying MCP handshake to new server")
}

type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    map[string]struct {
		ListChanged bool `json:"listChanged"`
	} `json:"capabilities"`
}

func (p *proxy) finishReplay(env jsonrpc.Envelope) {
	stopTimer(&p.initTimer)
	p.replayID = ""
	if env.IsError {
		p.log.Error("restarted server rejected the replayed initialize")
		p.killChild("replayed initialize rejected")
		return
	}
	var res initResult
	_ = json.Unmarshal(env.Result, &res)
	if p.initVersion != "" && res.ProtocolVersion != p.initVersion {
		p.log.Warn("restarted server negotiated a different protocol version",
			"was", p.initVersion, "now", res.ProtocolVersion)
	}

	initialized := p.initializedMsg
	if initialized == nil {
		initialized = jsonrpc.NewNotification("notifications/initialized")
	}
	p.toChild.Push(initialized)
	p.setPhase(state.Running)
	p.flushBuffer()

	// Tell the client to re-fetch anything that may differ in the new server.
	for _, kind := range []string{"tools", "prompts", "resources"} {
		if res.Capabilities[kind].ListChanged {
			p.toClient.Push(jsonrpc.NewNotification("notifications/" + kind + "/list_changed"))
		}
	}
	p.log.Info("session restored", "generation", p.gen)
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// ---------------------------------------------------------------------------
// Client → server

func (p *proxy) handleClient(line []byte) {
	env := jsonrpc.Parse(line)
	p.count(env)
	p.capture(env, line)

	switch env.Kind {
	case jsonrpc.Response:
		// The client answering a server→client request. It only makes sense
		// to the generation that asked.
		key := env.Key()
		gen, known := p.serverReqs[key]
		delete(p.serverReqs, key)
		if p.phase != state.Running || (known && gen != p.gen) {
			p.log.Debug("dropping client response for a previous server generation", "id", key)
			return
		}
		p.toChild.Push(line)
		return
	case jsonrpc.Notification:
		if env.Method == "notifications/cancelled" {
			delete(p.inflight, jsonrpc.IDKey(jsonrpc.CancelledRequestID(env.Params)))
		}
	}

	switch p.phase {
	case state.Running:
		p.forward(env, line)
	case state.Failed:
		p.rejectIfRequest(env, "MCP server failed and will not be restarted", ReasonCircuitOpen)
	default: // starting / restarting
		if len(p.buffer) >= p.cfg.BufferSize {
			p.rejectIfRequest(env, "MCP server is restarting and the request buffer is full", ReasonBufferFull)
			return
		}
		p.buffer = append(p.buffer, line)
	}
}

func (p *proxy) count(env jsonrpc.Envelope) {
	switch env.Kind {
	case jsonrpc.Request:
		p.cfg.Metrics.Request()
	case jsonrpc.Notification:
		p.cfg.Metrics.Notification()
	case jsonrpc.Batch:
		for _, e := range env.Batch {
			p.count(e)
		}
	}
}

// capture records the handshake so it can be replayed after a restart.
func (p *proxy) capture(env jsonrpc.Envelope, line []byte) {
	switch {
	case env.Kind == jsonrpc.Request && env.Method == "initialize":
		p.initParams = env.Params
		p.initKey = env.Key()
		p.initVersion = ""
		p.sessionUp = false
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(env.Params, &params)
		p.initVersion = params.ProtocolVersion
	case env.Kind == jsonrpc.Notification && env.Method == "notifications/initialized":
		p.initializedMsg = line
	}
}

func (p *proxy) forward(env jsonrpc.Envelope, line []byte) {
	p.trackRequests(env)
	p.toChild.Push(line)
}

func (p *proxy) trackRequests(env jsonrpc.Envelope) {
	switch env.Kind {
	case jsonrpc.Request:
		p.inflight[env.Key()] = inflight{id: env.ID, method: env.Method, start: time.Now()}
	case jsonrpc.Batch:
		for _, e := range env.Batch {
			p.trackRequests(e)
		}
	}
}

func (p *proxy) flushBuffer() {
	buf := p.buffer
	p.buffer = nil
	for _, line := range buf {
		p.forward(jsonrpc.Parse(line), line)
	}
}

func (p *proxy) rejectIfRequest(env jsonrpc.Envelope, msg, reason string) {
	switch env.Kind {
	case jsonrpc.Request:
		p.cfg.Metrics.Error()
		p.toClient.Push(jsonrpc.ErrorResponse(env.ID, jsonrpc.CodeInternalError, msg,
			map[string]string{"reason": reason}))
	case jsonrpc.Batch:
		for _, e := range env.Batch {
			p.rejectIfRequest(e, msg, reason)
		}
	}
}

var idPattern = regexp.MustCompile(`"id"\s*:\s*("(?:[^"\\]|\\.)*"|-?\d+)`)

func recoverID(err error) json.RawMessage {
	var tl *jsonrpc.TooLongError
	if !errors.As(err, &tl) {
		return nil
	}
	m := idPattern.FindSubmatch(tl.Prefix)
	if m == nil {
		return nil
	}
	return m[1]
}

func (p *proxy) handleClientFramingError(err error) {
	p.cfg.Metrics.Error()
	p.log.Error("client message too large; dropped", "err", err, "max", p.cfg.MaxMessage)
	if id := recoverID(err); id != nil {
		p.toClient.Push(jsonrpc.ErrorResponse(id, jsonrpc.CodeInvalidRequest,
			fmt.Sprintf("message exceeds mcp-warden's %d byte limit", p.cfg.MaxMessage),
			map[string]string{"reason": ReasonTooLarge}))
	}
}

// ---------------------------------------------------------------------------
// Server → client

func (p *proxy) handleServer(line []byte) {
	env := jsonrpc.Parse(line)
	switch env.Kind {
	case jsonrpc.Response:
		key := env.Key()
		if p.replayID != "" && key == p.replayID {
			p.finishReplay(env) // swallowed: the client never sent this request
			return
		}
		p.completeRequest(env)
		if key == p.initKey && !env.IsError {
			p.sessionUp = true
		}
	case jsonrpc.Request:
		p.serverReqs[env.Key()] = p.gen
	case jsonrpc.Batch:
		for _, e := range env.Batch {
			if e.Kind == jsonrpc.Response {
				p.completeRequest(e)
			}
		}
	case jsonrpc.Invalid:
		p.cfg.Metrics.Error()
		p.log.Warn("server sent a malformed message; relaying unchanged", "bytes", len(line))
	}
	p.toClient.Push(line)
}

func (p *proxy) completeRequest(env jsonrpc.Envelope) {
	if f, ok := p.inflight[env.Key()]; ok {
		delete(p.inflight, env.Key())
		p.cfg.Metrics.Observe(time.Since(f.start))
	}
	if env.IsError {
		p.cfg.Metrics.Error()
	}
}

func (p *proxy) handleServerFramingError(err error) {
	p.cfg.Metrics.Error()
	p.log.Error("server message too large; dropped", "err", err, "max", p.cfg.MaxMessage)
	// If it was a response, answer the waiting request so the client does
	// not hang forever.
	id := recoverID(err)
	if id == nil {
		return
	}
	if f, ok := p.inflight[jsonrpc.IDKey(id)]; ok {
		delete(p.inflight, jsonrpc.IDKey(id))
		p.toClient.Push(jsonrpc.ErrorResponse(f.id, jsonrpc.CodeInternalError,
			fmt.Sprintf("server response exceeded mcp-warden's %d byte limit", p.cfg.MaxMessage),
			map[string]string{"reason": ReasonTooLarge}))
	}
}

// ---------------------------------------------------------------------------
// Status

func (p *proxy) setPhase(ph state.Phase) {
	p.phase = ph
	if p.cfg.OnStatus == nil {
		return
	}
	s := Status{
		Phase:         ph,
		Restarts:      p.restarts,
		LastRestartAt: p.lastRestartAt,
		LastExit:      p.lastExit,
	}
	if p.child != nil {
		s.ChildPID = p.child.Pid()
		s.ChildStartedAt = p.childStarted
	}
	p.cfg.OnStatus(s)
}

// StatusRecorder is a concurrency-safe holder for the latest Status, for
// callers that publish it on their own schedule.
type StatusRecorder struct {
	mu sync.Mutex
	s  Status
	ch chan struct{}
}

// NewStatusRecorder returns a recorder; Changed fires (coalesced) on updates.
func NewStatusRecorder() *StatusRecorder {
	return &StatusRecorder{ch: make(chan struct{}, 1)}
}

// Set stores s. It never blocks, so it is safe to use as Config.OnStatus.
func (r *StatusRecorder) Set(s Status) {
	r.mu.Lock()
	r.s = s
	r.mu.Unlock()
	select {
	case r.ch <- struct{}{}:
	default:
	}
}

// Get returns the latest Status.
func (r *StatusRecorder) Get() Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.s
}

// Changed signals after Set.
func (r *StatusRecorder) Changed() <-chan struct{} { return r.ch }
