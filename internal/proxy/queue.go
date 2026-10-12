package proxy

import (
	"io"
	"sync"
)

// lineQueue is an unbounded FIFO of messages drained by one writer goroutine.
// Push never blocks, which is what keeps the actor loop from deadlocking on
// a peer that has stopped reading. Memory is bounded in practice by the
// restart buffer and the client's own request rate.
type lineQueue struct {
	mu     sync.Mutex
	items  [][]byte
	closed bool
	wake   chan struct{}
	done   chan struct{}
}

func newLineQueue() *lineQueue {
	return &lineQueue{wake: make(chan struct{}, 1), done: make(chan struct{})}
}

// Push enqueues a message. It reports false if the queue is closed.
func (q *lineQueue) Push(line []byte) bool {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return false
	}
	q.items = append(q.items, line)
	q.mu.Unlock()
	q.signal()
	return true
}

// Close stops accepting messages; queued ones are still written.
func (q *lineQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *lineQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Drain writes queued messages to w, newline-terminated, until the queue is
// closed and empty. After a write error the remaining messages are discarded
// (the peer is gone). If closer is non-nil it is closed at the end — for a
// child's stdin, that is the graceful-shutdown signal.
func (q *lineQueue) Drain(w io.Writer, closer io.Closer) {
	defer close(q.done)
	if closer != nil {
		defer closer.Close()
	}
	broken := false
	for {
		q.mu.Lock()
		batch := q.items
		q.items = nil
		closed := q.closed
		q.mu.Unlock()

		for _, line := range batch {
			if broken {
				continue
			}
			// One Write per message keeps lines atomic on pipes.
			buf := make([]byte, 0, len(line)+1)
			buf = append(append(buf, line...), '\n')
			if _, err := w.Write(buf); err != nil {
				broken = true
			}
		}
		if closed && len(batch) == 0 {
			return
		}
		if len(batch) == 0 {
			<-q.wake
		}
	}
}

// Done is closed once Drain has returned.
func (q *lineQueue) Done() <-chan struct{} { return q.done }
