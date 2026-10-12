package proxy

import (
	"bufio"
	"context"
	"io"
	"strconv"
	"testing"
	"time"
)

// BenchmarkRoundTrip measures one request/response through the proxy against
// an in-memory server, i.e. mcp-warden's own per-message overhead (two
// envelope parses, two queue hops, in-flight bookkeeping). SPEC-001 target:
// well under 1ms.
func BenchmarkRoundTrip(b *testing.B) {
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, Config{
			In: inR, Out: outW,
			Spawn: func() (Child, error) {
				c := &fakeChild{pid: 1, exited: make(chan struct{})}
				c.stdinR, c.stdinW = io.Pipe()
				c.stdoutR, c.stdoutW = io.Pipe()
				c.stderrR, c.stderrW = io.Pipe()
				go c.serve(behavior{})
				return c, nil
			},
			Grace: 100 * time.Millisecond,
		})
	}()
	out := bufio.NewScanner(outR)
	send := func(s string) {
		if _, err := io.WriteString(inW, s+"\n"); err != nil {
			b.Fatal(err)
		}
	}
	send(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{}}`)
	out.Scan()

	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		send(`{"jsonrpc":"2.0","id":` + strconv.Itoa(i+1) + `,"method":"echo","params":{"text":"hello"}}`)
		if !out.Scan() {
			b.Fatal("proxy closed stdout")
		}
	}
	b.StopTimer()
	inW.Close()
	<-done
}
