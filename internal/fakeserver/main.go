// Command fakeserver is a scriptable MCP server used by the end-to-end tests.
// It speaks just enough of the protocol to exercise mcp-warden, and its
// failure modes are selected with environment variables:
//
//	FAKE_CRASH_ON_START=1      exit 1 immediately
//	FAKE_GRANDCHILD_PIDFILE=p  spawn a long-lived child of itself, write its PID to p
//	FAKE_ORPHAN_PIDFILE=p      spawn a long-lived child, write its PID to p, and exit
//	                           (leaving the child orphaned)
//	FAKE_SLEEP=1               do nothing but sleep (used for spawned children)
//
// Every received method is logged to stderr as "fakeserver: recv <method>".
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func main() {
	if os.Getenv("FAKE_SLEEP") == "1" {
		time.Sleep(time.Hour)
		return
	}
	if os.Getenv("FAKE_CRASH_ON_START") == "1" {
		fmt.Fprintln(os.Stderr, "fakeserver: crashing on start")
		os.Exit(1)
	}
	if p := os.Getenv("FAKE_ORPHAN_PIDFILE"); p != "" {
		spawnSleeper(p)
		return // exit, abandoning the child
	}
	if p := os.Getenv("FAKE_GRANDCHILD_PIDFILE"); p != "" {
		spawnSleeper(p)
	}
	serve()
}

func spawnSleeper(pidfile string) {
	cmd := exec.Command(os.Args[0], "--role=sleeper", "mcp-test-orphan")
	cmd.Env = append(os.Environ(), "FAKE_SLEEP=1", "FAKE_ORPHAN_PIDFILE=", "FAKE_GRANDCHILD_PIDFILE=")
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "fakeserver:", err)
		os.Exit(2)
	}
	if err := os.WriteFile(pidfile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "fakeserver:", err)
		os.Exit(2)
	}
}

type message struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
}

func serve() {
	out := bufio.NewWriter(os.Stdout)
	reply := func(id json.RawMessage, result string) {
		fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"result":%s}`+"\n", id, result)
		out.Flush()
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var m message
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		fmt.Fprintf(os.Stderr, "fakeserver: recv %s\n", m.Method)
		if m.ID == nil {
			continue // notification
		}
		switch m.Method {
		case "initialize":
			reply(m.ID, `{"protocolVersion":"2025-06-18","capabilities":{"tools":{"listChanged":true}},"serverInfo":{"name":"fakeserver","version":"1"}}`)
		case "tools/list":
			reply(m.ID, `{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}`)
		case "tools/call":
			reply(m.ID, `{"content":[{"type":"text","text":"ok"}]}`)
		case "ping":
			reply(m.ID, `{}`)
		case "crash":
			os.Exit(1)
		default:
			fmt.Fprintf(out, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}`+"\n", m.ID)
			out.Flush()
		}
	}
}
