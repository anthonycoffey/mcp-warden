package jsonrpc

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func readAll(t *testing.T, r *Reader) ([]string, []error) {
	t.Helper()
	var lines []string
	var errs []error
	for {
		line, err := r.Next()
		if errors.Is(err, io.EOF) {
			return lines, errs
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lines = append(lines, string(line))
	}
}

func TestReader(t *testing.T) {
	big := strings.Repeat("x", 200_000) // larger than the internal 64 KiB buffer
	tests := []struct {
		name     string
		input    string
		max      int
		want     []string
		wantErrs int
	}{
		{"single", `{"a":1}` + "\n", 0, []string{`{"a":1}`}, 0},
		{"multiple", "a\nb\nc\n", 0, []string{"a", "b", "c"}, 0},
		{"crlf", "a\r\nb\r\n", 0, []string{"a", "b"}, 0},
		{"blank lines skipped", "\n\na\n  \nb\n", 0, []string{"a", "b"}, 0},
		{"unterminated final line", "a\nb", 0, []string{"a", "b"}, 0},
		{"larger than buffer", big + "\nz\n", 0, []string{big, "z"}, 0},
		{"too long is skipped", "ok\n" + big + "\nnext\n", 1000, []string{"ok", "next"}, 1},
		{"too long within buffer", "ok\n" + strings.Repeat("y", 50) + "\nnext\n", 10, []string{"ok", "next"}, 1},
		{"exactly max", "12345\n", 5, []string{"12345"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, errs := readAll(t, NewReader(strings.NewReader(tt.input), tt.max))
			if len(errs) != tt.wantErrs {
				t.Fatalf("errors = %v, want %d", errs, tt.wantErrs)
			}
			for _, err := range errs {
				if !errors.Is(err, ErrTooLong) {
					t.Errorf("error %v is not ErrTooLong", err)
				}
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("lines = %q, want %q", truncate(got), truncate(tt.want))
			}
		})
	}
}

func TestTooLongErrorKeepsPrefix(t *testing.T) {
	r := NewReader(strings.NewReader(`{"id":42,"method":"x","params":"`+strings.Repeat("p", 500)+"\"}\n"), 100)
	_, err := r.Next()
	var tl *TooLongError
	if !errors.As(err, &tl) {
		t.Fatalf("err = %v, want *TooLongError", err)
	}
	if !strings.HasPrefix(string(tl.Prefix), `{"id":42`) {
		t.Errorf("prefix = %q", tl.Prefix)
	}
}

func truncate(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		if len(v) > 20 {
			v = v[:20] + "…"
		}
		out[i] = v
	}
	return out
}

func FuzzReader(f *testing.F) {
	f.Add([]byte("a\nb\r\n\n{\"x\":1}"), 8)
	f.Add([]byte(strings.Repeat("z", 300)), 64)
	f.Fuzz(func(t *testing.T, data []byte, max int) {
		if max < 1 || max > 1<<16 {
			return
		}
		r := NewReader(strings.NewReader(string(data)), max)
		for i := 0; i < len(data)+2; i++ {
			line, err := r.Next()
			if errors.Is(err, io.EOF) {
				return
			}
			if err == nil && len(line) > max+1 {
				t.Fatalf("returned line of %d bytes with max %d", len(line), max)
			}
		}
		t.Fatal("reader did not reach EOF")
	})
}

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		kind    Kind
		id      string
		method  string
		isError bool
	}{
		{"request numeric id", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, Request, "1", "tools/list", false},
		{"request string id", `{"jsonrpc":"2.0","id":"abc","method":"x","params":{}}`, Request, `"abc"`, "x", false},
		{"notification", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, Notification, "", "notifications/initialized", false},
		{"null id is notification", `{"jsonrpc":"2.0","id":null,"method":"n"}`, Notification, "", "n", false},
		{"result", `{"jsonrpc":"2.0","id":7,"result":{}}`, Response, "7", "", false},
		{"error", `{"jsonrpc":"2.0","id":7,"error":{"code":1,"message":"m"}}`, Response, "7", "", true},
		{"garbage", `not json`, Invalid, "", "", false},
		{"no method no result", `{"id":1}`, Invalid, "", "", false},
		{"empty", ``, Invalid, "", "", false},
		{"empty batch", `[]`, Invalid, "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := Parse([]byte(tt.in))
			if env.Kind != tt.kind {
				t.Fatalf("kind = %v, want %v", env.Kind, tt.kind)
			}
			if got := string(env.ID); got != tt.id {
				t.Errorf("id = %q, want %q", got, tt.id)
			}
			if env.Method != tt.method {
				t.Errorf("method = %q, want %q", env.Method, tt.method)
			}
			if env.IsError != tt.isError {
				t.Errorf("isError = %v, want %v", env.IsError, tt.isError)
			}
		})
	}
}

func TestParseBatch(t *testing.T) {
	env := Parse([]byte(`[{"jsonrpc":"2.0","id":1,"method":"a"},{"jsonrpc":"2.0","method":"b"}]`))
	if env.Kind != Batch || len(env.Batch) != 2 {
		t.Fatalf("got %+v", env)
	}
	if env.Batch[0].Kind != Request || env.Batch[1].Kind != Notification {
		t.Errorf("elements = %v, %v", env.Batch[0].Kind, env.Batch[1].Kind)
	}
}

func TestIDKey(t *testing.T) {
	if IDKey(json.RawMessage(`1`)) == IDKey(json.RawMessage(`"1"`)) {
		t.Error("numeric and string ids must not collide")
	}
	if IDKey(json.RawMessage(` "a" `)) != IDKey(json.RawMessage(`"a"`)) {
		t.Error("whitespace must not affect key")
	}
}

func TestBuilders(t *testing.T) {
	resp := Parse(ErrorResponse(json.RawMessage(`5`), CodeInternalError, "boom", map[string]string{"reason": "r"}))
	if resp.Kind != Response || !resp.IsError || string(resp.ID) != "5" {
		t.Errorf("ErrorResponse parsed as %+v", resp)
	}
	n := Parse(NewNotification("notifications/tools/list_changed"))
	if n.Kind != Notification || n.Method != "notifications/tools/list_changed" {
		t.Errorf("NewNotification parsed as %+v", n)
	}
	r := Parse(NewRequest("mcp-warden:init:2", "initialize", json.RawMessage(`{"a":1}`)))
	if r.Kind != Request || string(r.ID) != `"mcp-warden:init:2"` || string(r.Params) != `{"a":1}` {
		t.Errorf("NewRequest parsed as %+v", r)
	}
	if id := CancelledRequestID(json.RawMessage(`{"requestId":9,"reason":"x"}`)); string(id) != "9" {
		t.Errorf("CancelledRequestID = %s", id)
	}
}
