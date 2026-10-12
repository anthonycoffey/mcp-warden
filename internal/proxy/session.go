package proxy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/anthonycoffey/mcp-warden/internal/jsonrpc"
	"github.com/anthonycoffey/mcp-warden/internal/state"
)

// MCP is stateful: a server only accepts requests after the initialize /
// notifications/initialized handshake. When a new generation starts after
// the client already completed that handshake, the proxy replays it on the
// client's behalf, then releases the buffered client traffic.

// initResult is the part of an initialize result the proxy cares about.
type initResult struct {
	ProtocolVersion string `json:"protocolVersion"`
	Capabilities    map[string]struct {
		ListChanged bool `json:"listChanged"`
	} `json:"capabilities"`
}

func parseInitResult(raw json.RawMessage) initResult {
	var res initResult
	_ = json.Unmarshal(raw, &res) // a malformed result just means no capabilities
	return res
}

// startReplay sends the client's original initialize, under a reserved id,
// to a freshly started generation.
func (p *proxy) startReplay() {
	p.setPhase(state.Restarting) // still restarting until the session is restored
	id := fmt.Sprintf("%sinit:%d", reservedIDPrefix, p.gen)
	idJSON, _ := json.Marshal(id)
	p.replayID = jsonrpc.IDKey(idJSON)
	p.toChild.Push(jsonrpc.NewRequest(id, "initialize", p.initParams))
	p.initTimer = time.NewTimer(p.cfg.InitTimeout)
	p.log.Info("replaying MCP handshake to new server")
}

// finishReplay handles the response to the replayed initialize. The client
// never sees that response: it never sent the request.
func (p *proxy) finishReplay(env jsonrpc.Envelope) {
	stopTimer(&p.initTimer)
	p.replayID = ""
	if env.IsError {
		p.log.Error("restarted server rejected the replayed initialize")
		p.killChild("replayed initialize rejected")
		return
	}
	res := parseInitResult(env.Result)
	if p.negotiatedVersion != "" && res.ProtocolVersion != p.negotiatedVersion {
		p.log.Warn("restarted server negotiated a different protocol version",
			"was", p.negotiatedVersion, "now", res.ProtocolVersion)
	}

	initialized := p.initializedMsg
	if initialized == nil {
		initialized = jsonrpc.NewNotification("notifications/initialized")
	}
	p.toChild.Push(initialized)
	p.setPhase(state.Running)
	p.flushBuffer()

	// The new process may expose different tools, prompts or resources;
	// ask the client to re-fetch whichever lists the server says can change.
	for _, kind := range []string{"tools", "prompts", "resources"} {
		if res.Capabilities[kind].ListChanged {
			p.toClient.Push(jsonrpc.NewNotification("notifications/" + kind + "/list_changed"))
		}
	}
	p.log.Info("session restored", "generation", p.gen)
}
