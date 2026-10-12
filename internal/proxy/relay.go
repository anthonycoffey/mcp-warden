package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/jsonrpc"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

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
		// A new initialize starts a new session.
		p.initParams = env.Params
		p.initKey = env.Key()
		p.negotiatedVersion = ""
		p.sessionUp = false
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
			// The client's handshake succeeded: from now on a restart
			// must replay it.
			p.sessionUp = true
			p.negotiatedVersion = parseInitResult(env.Result).ProtocolVersion
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
