package jsonrpc

import (
	"bytes"
	"encoding/json"
)

// Kind classifies a JSON-RPC message.
type Kind int

const (
	// Invalid is anything that is not a well-formed JSON-RPC object or batch.
	Invalid Kind = iota
	// Request has a method and an id and expects a response.
	Request
	// Notification has a method and no id.
	Notification
	// Response has an id and a result or error.
	Response
	// Batch is a JSON array of messages (allowed by older MCP revisions).
	Batch
)

func (k Kind) String() string {
	switch k {
	case Request:
		return "request"
	case Notification:
		return "notification"
	case Response:
		return "response"
	case Batch:
		return "batch"
	default:
		return "invalid"
	}
}

// Standard JSON-RPC error codes used by mcp-warden.
const (
	CodeInvalidRequest = -32600
	CodeInternalError  = -32603
)

// Envelope is the routing-relevant part of a message. The original bytes are
// always what gets forwarded; Envelope exists only so the proxy can make
// decisions without re-encoding anything.
type Envelope struct {
	Kind    Kind
	ID      json.RawMessage // raw id exactly as sent, nil for notifications
	Method  string
	IsError bool            // response carries "error" rather than "result"
	Result  json.RawMessage // raw result, responses only
	Params  json.RawMessage // raw params, requests and notifications only
	Batch   []Envelope      // elements, when Kind == Batch
}

// Key returns a map key for the id. Two ids are the same key when their
// compact JSON encodings are identical (so 1 and "1" stay distinct).
func (e Envelope) Key() string { return IDKey(e.ID) }

// IDKey normalizes a raw id into a comparable key.
func IDKey(id json.RawMessage) string {
	var b bytes.Buffer
	if err := json.Compact(&b, id); err != nil {
		return string(id)
	}
	return b.String()
}

type wireMessage struct {
	ID     json.RawMessage `json:"id"`
	Method *string         `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  json.RawMessage `json:"error"`
	Params json.RawMessage `json:"params"`
}

// Parse classifies a single framed message. It never returns an error;
// anything it cannot understand is Kind Invalid.
func Parse(line []byte) Envelope {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return Envelope{Kind: Invalid}
	}
	if trimmed[0] == '[' {
		var raw []json.RawMessage
		if err := json.Unmarshal(trimmed, &raw); err != nil || len(raw) == 0 {
			return Envelope{Kind: Invalid}
		}
		env := Envelope{Kind: Batch, Batch: make([]Envelope, 0, len(raw))}
		for _, r := range raw {
			env.Batch = append(env.Batch, Parse(r))
		}
		return env
	}
	var m wireMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return Envelope{Kind: Invalid}
	}
	hasID := len(m.ID) > 0 && !bytes.Equal(m.ID, []byte("null"))
	switch {
	case m.Method != nil && hasID:
		return Envelope{Kind: Request, ID: m.ID, Method: *m.Method, Params: m.Params}
	case m.Method != nil:
		return Envelope{Kind: Notification, Method: *m.Method, Params: m.Params}
	case hasID && (m.Result != nil || m.Error != nil):
		return Envelope{Kind: Response, ID: m.ID, IsError: m.Error != nil, Result: m.Result}
	default:
		return Envelope{Kind: Invalid}
	}
}

type errorObject struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ErrorResponse builds a JSON-RPC error response for id.
func ErrorResponse(id json.RawMessage, code int, message string, data any) []byte {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	b, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Error   errorObject     `json:"error"`
	}{"2.0", id, errorObject{code, message, data}})
	return b
}

// NewNotification builds a parameterless notification.
func NewNotification(method string) []byte {
	b, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
	}{"2.0", method})
	return b
}

// NewRequest builds a request with a string id and raw params.
func NewRequest(id, method string, params json.RawMessage) []byte {
	idRaw, _ := json.Marshal(id)
	b, _ := json.Marshal(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params,omitempty"`
	}{"2.0", idRaw, method, params})
	return b
}

// CancelledRequestID extracts params.requestId from a notifications/cancelled
// message, or nil if absent.
func CancelledRequestID(params json.RawMessage) json.RawMessage {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil {
		return nil
	}
	return p.RequestID
}
