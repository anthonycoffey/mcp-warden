package proxy

import (
	"bufio"
	"errors"
	"io"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/jsonrpc"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

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
