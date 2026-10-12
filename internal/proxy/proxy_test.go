package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/jsonrpc"
	"github.com/anthonycoffey/mcp-warden/internal/state"
	"github.com/anthonycoffey/mcp-warden/internal/supervisor"
)

// ---------------------------------------------------------------------------
// In-memory fake MCP server

// behavior scripts one generation of the fake server.
type behavior struct {
	crashOnStart bool // exit immediately
	ignoreInit   bool // never answer initialize
	ignoreEOF    bool // keep running after stdin closes
}

type fakeChild struct {
	pid         int
	stdinR      *io.PipeReader
	stdinW      *io.PipeWriter
	stdoutR     *io.PipeReader
	stdoutW     *io.PipeWriter
	stderrR     *io.PipeReader
	stderrW     *io.PipeWriter
	exited      chan struct{}
	once        sync.Once
	exitErr     error
	mu          sync.Mutex
	received    []string // methods (or "response:<id>") in arrival order
	receivedIDs []string
}

func (c *fakeChild) Stdin() io.WriteCloser { return c.stdinW }
func (c *fakeChild) Stdout() io.Reader     { return c.stdoutR }
func (c *fakeChild) Stderr() io.Reader     { return c.stderrR }
func (c *fakeChild) Pid() int              { return c.pid }
func (c *fakeChild) Wait() error           { <-c.exited; return c.exitErr }
func (c *fakeChild) Kill() error           { c.exit(errors.New("killed")); return nil }
func (c *fakeChild) Close() error          { return nil }

func (c *fakeChild) exit(err error) {
	c.once.Do(func() {
		c.exitErr = err
		c.stdoutW.Close()
		c.stderrW.Close()
		c.stdinR.CloseWithError(io.ErrClosedPipe)
		close(c.exited)
	})
}

func (c *fakeChild) send(s string) { _, _ = c.stdoutW.Write([]byte(s + "\n")) }

func (c *fakeChild) Received() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.received)
}

func (c *fakeChild) firstID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.receivedIDs[0]
}

func (c *fakeChild) serve(b behavior) {
	if b.crashOnStart {
		c.exit(errors.New("exit status 1"))
		return
	}
	fmt.Fprintln(c.stderrW, "fake server ready")
	sc := bufio.NewScanner(c.stdinR)
	for sc.Scan() {
		env := jsonrpc.Parse(sc.Bytes())
		c.mu.Lock()
		if env.Kind == jsonrpc.Response {
			c.received = append(c.received, "response:"+env.Key())
		} else {
			c.received = append(c.received, env.Method)
		}
		c.receivedIDs = append(c.receivedIDs, env.Key())
		c.mu.Unlock()

		id := string(env.ID)
		switch env.Method {
		case "initialize":
			if !b.ignoreInit {
				c.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true},"resources":{}},"serverInfo":{"name":"fake"}}}`)
			}
		case "tools/list":
			c.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{"tools":[{"name":"echo"}]}}`)
		case "echo":
			c.send(`{"jsonrpc":"2.0","id":` + id + `,"result":` + string(env.Params) + `}`)
		case "ask": // server→client request, then answer the call
			c.send(`{"jsonrpc":"2.0","id":"srv-1","method":"roots/list"}`)
			c.send(`{"jsonrpc":"2.0","id":` + id + `,"result":{}}`)
		case "garbage":
			c.send(`this is not json`)
		case "crash":
			c.exit(errors.New("exit status 1"))
			return
		case "slow":
			// never answers
		}
	}
	if b.ignoreEOF {
		<-c.exited
		return
	}
	c.exit(nil)
}

// ---------------------------------------------------------------------------
// Harness

type harness struct {
	t        *testing.T
	in       *io.PipeWriter
	out      chan string
	children []*fakeChild
	mu       sync.Mutex
	done     chan error
	status   *StatusRecorder
	phases   chan state.Phase
}

func start(t *testing.T, behaviors func(gen int) behavior, tweak func(*Config)) *harness {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	h := &harness{t: t, in: inW, out: make(chan string, 256), done: make(chan error, 1),
		status: NewStatusRecorder(), phases: make(chan state.Phase, 64)}

	cfg := Config{
		In:  inR,
		Out: outW,
		Spawn: func() (Child, error) {
			h.mu.Lock()
			gen := len(h.children) + 1
			c := &fakeChild{pid: 1000 + gen, exited: make(chan struct{})}
			c.stdinR, c.stdinW = io.Pipe()
			c.stdoutR, c.stdoutW = io.Pipe()
			c.stderrR, c.stderrW = io.Pipe()
			h.children = append(h.children, c)
			h.mu.Unlock()
			go c.serve(behaviors(gen))
			return c, nil
		},
		OnStatus: func(s Status) {
			h.status.Set(s)
			select {
			case h.phases <- s.Phase:
			default:
			}
		},
		Backoff:       supervisor.Backoff{Base: 5 * time.Millisecond, Max: 20 * time.Millisecond, Factor: 2},
		RestartWindow: time.Minute,
		InitTimeout:   time.Second,
		Grace:         200 * time.Millisecond,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(nil, 1<<20)
		for sc.Scan() {
			h.out <- sc.Text()
		}
	}()
	go func() { h.done <- Run(context.Background(), cfg) }()
	t.Cleanup(func() {
		inW.Close()
		select {
		case <-h.done:
		case <-time.After(5 * time.Second):
			t.Error("proxy did not shut down")
		}
	})
	return h
}

func always(b behavior) func(int) behavior { return func(int) behavior { return b } }

func (h *harness) child(gen int) *fakeChild {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		if len(h.children) >= gen {
			c := h.children[gen-1]
			h.mu.Unlock()
			return c
		}
		h.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("generation %d never spawned", gen)
	return nil
}

func (h *harness) send(s string) {
	h.t.Helper()
	if _, err := h.in.Write([]byte(s + "\n")); err != nil {
		h.t.Fatal(err)
	}
}

// next returns the next message to the client.
func (h *harness) next() string {
	h.t.Helper()
	select {
	case s := <-h.out:
		return s
	case <-time.After(5 * time.Second):
		h.t.Fatal("timed out waiting for a message to the client")
		return ""
	}
}

// expectID waits for the response with the given raw id, collecting anything
// else that arrives first.
func (h *harness) expectID(id string) (jsonrpc.Envelope, string, []string) {
	h.t.Helper()
	var other []string
	for {
		s := h.next()
		env := jsonrpc.Parse([]byte(s))
		if env.Kind == jsonrpc.Response && string(env.ID) == id {
			return env, s, other
		}
		other = append(other, s)
	}
}

func (h *harness) waitPhase(want state.Phase) {
	h.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case p := <-h.phases:
			if p == want {
				return
			}
		case <-timeout:
			h.t.Fatalf("never reached phase %s (now %s)", want, h.status.Get().Phase)
		}
	}
}

func (h *harness) handshake() {
	h.t.Helper()
	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test"}}}`)
	h.expectID("1")
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

func reason(t *testing.T, line string) string {
	t.Helper()
	var m struct {
		Error struct {
			Data struct {
				Reason string `json:"reason"`
			} `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &m); err != nil {
		t.Fatalf("bad json %q: %v", line, err)
	}
	return m.Error.Data.Reason
}

// ---------------------------------------------------------------------------
// Tests

func TestRelaysBytesUnchanged(t *testing.T) {
	h := start(t, always(behavior{}), nil)
	h.handshake()
	// Odd spacing and key order must survive untouched.
	h.send(`{ "method" : "echo", "jsonrpc":"2.0", "id": 2, "params": {"b":1,  "a":[1,2]} }`)
	_, raw, _ := h.expectID("2")
	if raw != `{"jsonrpc":"2.0","id":2,"result":{"b":1,  "a":[1,2]}}` {
		t.Errorf("response = %s", raw)
	}
	if got := h.child(1).Received(); !slices.Equal(got, []string{"initialize", "notifications/initialized", "echo"}) {
		t.Errorf("server received %v", got)
	}
}

func TestCrashFailsInflightThenRestoresSession(t *testing.T) {
	h := start(t, always(behavior{}), nil)
	h.handshake()

	h.send(`{"jsonrpc":"2.0","id":7,"method":"crash"}`)
	env, raw, _ := h.expectID("7")
	if !env.IsError || reason(t, raw) != ReasonServerRestarted {
		t.Fatalf("in-flight request got %s", raw)
	}

	h.send(`{"jsonrpc":"2.0","id":8,"method":"tools/list"}`)
	env, _, other := h.expectID("8")
	if env.IsError {
		t.Fatal("request after restart failed")
	}

	// The new server got the replayed handshake before the client's request.
	got := h.child(2).Received()
	if !slices.Equal(got, []string{"initialize", "notifications/initialized", "tools/list"}) {
		t.Errorf("gen 2 received %v", got)
	}
	if id := h.child(2).firstID(); !strings.HasPrefix(id, `"mcp-warden:init:`) {
		t.Errorf("replayed initialize used id %s", id)
	}
	// The replayed initialize response was swallowed, and list_changed was
	// sent for tools (listChanged: true) but not resources (absent).
	all := slices.Concat(other, h.drain())
	if !slices.ContainsFunc(all, func(s string) bool { return strings.Contains(s, "notifications/tools/list_changed") }) {
		t.Errorf("no tools/list_changed in %v", all)
	}
	for _, s := range all {
		if strings.Contains(s, "mcp-warden:init") || strings.Contains(s, "resources/list_changed") {
			t.Errorf("unexpected message to client: %s", s)
		}
	}
	if s := h.status.Get(); s.Restarts != 1 || s.Phase != state.Running {
		t.Errorf("status = %+v", s)
	}
}

// drain returns whatever reaches the client in the next short while.
func (h *harness) drain() []string {
	var out []string
	for {
		select {
		case s := <-h.out:
			out = append(out, s)
		case <-time.After(100 * time.Millisecond):
			return out
		}
	}
}

func TestCircuitBreakerOpens(t *testing.T) {
	h := start(t, func(gen int) behavior { return behavior{crashOnStart: gen > 1} },
		func(c *Config) { c.MaxRestarts = 2 })
	h.handshake()
	h.send(`{"jsonrpc":"2.0","id":2,"method":"crash"}`)
	h.expectID("2")
	h.waitPhase(state.Failed)

	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	_, raw, _ := h.expectID("3")
	if reason(t, raw) != ReasonCircuitOpen {
		t.Errorf("after circuit opened got %s", raw)
	}
	if s := h.status.Get(); s.Restarts != 2 {
		t.Errorf("restarts = %d, want 2", s.Restarts)
	}
}

func TestRestartBufferBoundsAndFlushes(t *testing.T) {
	h := start(t, always(behavior{}), func(c *Config) {
		c.BufferSize = 1
		c.Backoff = supervisor.Backoff{Base: 300 * time.Millisecond, Factor: 1}
	})
	h.handshake()
	h.send(`{"jsonrpc":"2.0","id":2,"method":"crash"}`)
	h.expectID("2")
	h.waitPhase(state.Restarting)

	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`) // buffered
	h.send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`) // overflow
	_, raw, _ := h.expectID("4")
	if reason(t, raw) != ReasonBufferFull {
		t.Errorf("overflow got %s", raw)
	}
	env, _, _ := h.expectID("3")
	if env.IsError {
		t.Error("buffered request should succeed after restart")
	}
}

func TestLateClientResponseToDeadServerIsDropped(t *testing.T) {
	h := start(t, always(behavior{}), nil)
	h.handshake()
	h.send(`{"jsonrpc":"2.0","id":2,"method":"ask"}`)
	_, _, other := h.expectID("2")
	if len(other) != 1 || !strings.Contains(other[0], "roots/list") {
		t.Fatalf("expected the server→client request first, got %v", other)
	}

	h.send(`{"jsonrpc":"2.0","id":3,"method":"crash"}`)
	h.expectID("3")
	h.send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	h.expectID("4") // gen 2 is up

	// The client finally answers gen 1's request.
	h.send(`{"jsonrpc":"2.0","id":"srv-1","result":{"roots":[]}}`)
	h.send(`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	h.expectID("5")
	if got := h.child(2).Received(); slices.Contains(got, `response:"srv-1"`) {
		t.Errorf("gen 2 received a response meant for gen 1: %v", got)
	}
}

func TestClientResponseToLiveServerIsForwarded(t *testing.T) {
	h := start(t, always(behavior{}), nil)
	h.handshake()
	h.send(`{"jsonrpc":"2.0","id":2,"method":"ask"}`)
	h.expectID("2")
	h.send(`{"jsonrpc":"2.0","id":"srv-1","result":{"roots":[]}}`)
	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	h.expectID("3")
	if got := h.child(1).Received(); !slices.Contains(got, `response:"srv-1"`) {
		t.Errorf("server did not receive the client's response: %v", got)
	}
}

func TestCancelledRequestGetsNoRestartError(t *testing.T) {
	h := start(t, always(behavior{}), nil)
	h.handshake()
	h.send(`{"jsonrpc":"2.0","id":9,"method":"slow"}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":9}}`)
	h.send(`{"jsonrpc":"2.0","id":10,"method":"crash"}`)
	_, _, other := h.expectID("10")
	for _, s := range append(other, h.drain()...) {
		if env := jsonrpc.Parse([]byte(s)); env.Kind == jsonrpc.Response && string(env.ID) == "9" {
			t.Errorf("cancelled request 9 got a response: %s", s)
		}
	}
}

func TestShutdownOnClientEOF(t *testing.T) {
	h := start(t, always(behavior{}), nil)
	h.handshake()
	c := h.child(1)
	h.in.Close()
	select {
	case err := <-h.done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
		h.done <- nil // let Cleanup see it
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after EOF")
	}
	if c.exitErr != nil {
		t.Errorf("server should exit gracefully on EOF, got %v", c.exitErr)
	}
	if h.status.Get().Phase != state.Stopped {
		t.Errorf("final phase = %s", h.status.Get().Phase)
	}
}

func TestShutdownKillsAfterGrace(t *testing.T) {
	h := start(t, always(behavior{ignoreEOF: true}), func(c *Config) { c.Grace = 50 * time.Millisecond })
	h.handshake()
	c := h.child(1)
	begin := time.Now()
	h.in.Close()
	select {
	case <-h.done:
		h.done <- nil
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
	if c.exitErr == nil || c.exitErr.Error() != "killed" {
		t.Errorf("server exit = %v, want killed", c.exitErr)
	}
	if d := time.Since(begin); d < 50*time.Millisecond {
		t.Errorf("killed after %v, before the grace period", d)
	}
}

func TestReplayTimeoutCountsAsCrash(t *testing.T) {
	h := start(t, func(gen int) behavior { return behavior{ignoreInit: gen == 2} },
		func(c *Config) { c.InitTimeout = 100 * time.Millisecond })
	h.handshake()
	h.send(`{"jsonrpc":"2.0","id":2,"method":"crash"}`)
	h.expectID("2")
	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	env, _, _ := h.expectID("3") // served by gen 3
	if env.IsError {
		t.Fatal("request should be served once a healthy generation is up")
	}
	h.child(3)
	if s := h.status.Get(); s.Restarts != 2 || s.LastExit != "initialize replay timed out" {
		t.Errorf("status = %+v", s)
	}
}

func TestOversizedClientMessageIsRejected(t *testing.T) {
	h := start(t, always(behavior{}), func(c *Config) { c.MaxMessage = 200 })
	h.handshake()
	h.send(`{"jsonrpc":"2.0","id":3,"method":"echo","params":"` + strings.Repeat("x", 500) + `"}`)
	_, raw, _ := h.expectID("3")
	if reason(t, raw) != ReasonTooLarge {
		t.Errorf("got %s", raw)
	}
	h.send(`{"jsonrpc":"2.0","id":4,"method":"tools/list"}`)
	h.expectID("4") // stream still healthy
}

func TestMalformedServerOutputIsRelayed(t *testing.T) {
	h := start(t, always(behavior{}), nil)
	h.handshake()
	h.send(`{"jsonrpc":"2.0","method":"garbage"}`)
	if got := h.next(); got != "this is not json" {
		t.Errorf("got %q", got)
	}
}

func TestSpawnFailureIsReported(t *testing.T) {
	inR, _ := io.Pipe()
	err := Run(context.Background(), Config{
		In: inR, Out: io.Discard,
		Spawn: func() (Child, error) { return nil, errors.New("executable not found") },
	})
	var se *SpawnError
	if !errors.As(err, &se) {
		t.Fatalf("Run = %v, want *SpawnError", err)
	}
}

func TestContextCancelStops(t *testing.T) {
	inR, _ := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var c *fakeChild
	go func() {
		done <- Run(ctx, Config{In: inR, Out: io.Discard, Grace: 50 * time.Millisecond,
			Spawn: func() (Child, error) {
				c = &fakeChild{pid: 1, exited: make(chan struct{})}
				c.stdinR, c.stdinW = io.Pipe()
				c.stdoutR, c.stdoutW = io.Pipe()
				c.stderrR, c.stderrW = io.Pipe()
				go c.serve(behavior{})
				return c, nil
			}})
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run ignored context cancellation")
	}
}
