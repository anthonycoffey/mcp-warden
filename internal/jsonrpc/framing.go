// Package jsonrpc implements the small slice of JSON-RPC 2.0 that mcp-warden
// needs: newline-delimited framing (the MCP stdio transport) and a cheap
// envelope parse that classifies a message without re-encoding it.
package jsonrpc

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"slices"
)

// DefaultMaxMessage is the default upper bound on a single message, in bytes.
const DefaultMaxMessage = 16 << 20

// ErrTooLong is returned by Reader.Next when a line exceeds the configured
// maximum. The oversized line has already been discarded, so the Reader is
// still usable and the next call returns the following message.
var ErrTooLong = errors.New("jsonrpc: message exceeds maximum size")

// TooLongError wraps ErrTooLong with the bytes that were seen before the limit
// was hit, which callers may use to recover a request id.
type TooLongError struct {
	Prefix []byte
	Size   int
}

func (e *TooLongError) Error() string {
	return fmt.Sprintf("jsonrpc: message of at least %d bytes exceeds maximum size", e.Size)
}

func (e *TooLongError) Unwrap() error { return ErrTooLong }

// Reader splits a stream into newline-delimited messages. Unlike
// bufio.Scanner it has no fixed token limit below max, and an oversized line
// is reported and skipped rather than ending the stream.
type Reader struct {
	r   *bufio.Reader
	max int
}

// NewReader returns a Reader that rejects messages larger than max bytes.
// A max <= 0 selects DefaultMaxMessage.
func NewReader(r io.Reader, max int) *Reader {
	if max <= 0 {
		max = DefaultMaxMessage
	}
	return &Reader{r: bufio.NewReaderSize(r, 64<<10), max: max}
}

// Next returns the next non-empty message with its line terminator removed.
// The returned slice is owned by the caller. At end of stream it returns
// io.EOF; a final unterminated line is returned before io.EOF.
func (r *Reader) Next() ([]byte, error) {
	for {
		line, err := r.readLine()
		if err != nil {
			return nil, err
		}
		line = bytes.TrimRight(line, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		return line, nil
	}
}

func (r *Reader) readLine() ([]byte, error) {
	var buf []byte
	for {
		chunk, err := r.r.ReadSlice('\n')
		if len(buf)+len(chunk) > r.max+1 { // +1 allows for the terminator
			prefix := slices.Concat(buf, chunk)
			if len(prefix) > 256 {
				prefix = prefix[:256]
			}
			size := len(buf) + len(chunk)
			if errors.Is(err, bufio.ErrBufferFull) {
				size += r.discardLine()
			}
			return nil, &TooLongError{Prefix: bytes.Clone(prefix), Size: size}
		}
		switch {
		case err == nil:
			return append(buf, chunk[:len(chunk)-1]...), nil
		case errors.Is(err, bufio.ErrBufferFull):
			buf = append(buf, chunk...)
		case errors.Is(err, io.EOF) && len(buf)+len(chunk) > 0:
			return append(buf, chunk...), nil
		default:
			return nil, err
		}
	}
}

// discardLine consumes input up to and including the next newline and
// reports how many bytes were dropped.
func (r *Reader) discardLine() int {
	n := 0
	for {
		chunk, err := r.r.ReadSlice('\n')
		n += len(chunk)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return n
		}
	}
}
