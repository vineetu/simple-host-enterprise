// Package mcp serves the Model Context Protocol over HTTP so an agent can drive
// Simple Host with tool calls instead of hand-written REST requests.
//
// Every tool is an adapter over the REST handler that already exists: a tool
// builds a request and serves it into the same mux the public API uses. Nothing
// here reimplements deploy, rollback, sharing or their guards, so MCP cannot
// drift from REST, and adding it required no change to the deploy path.
package mcp

import "encoding/json"

// JSON-RPC 2.0 error codes. The first four are the spec's; -32002 is MCP's
// convention for "the server is not ready for this yet".
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// Protocol-defined codes from the MCP reserved range.
const (
	// codeHeaderMismatch: the mirrored HTTP headers disagree with the body.
	codeHeaderMismatch = -32020
	// codeUnsupportedVersion: the requested revision is not served. A client
	// that speaks several eras uses the presence of THIS code to tell a modern
	// server from a legacy one, so emitting anything else sends it down the
	// wrong path.
	codeUnsupportedVersion = -32022
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the peer expects no reply. A notification has
// no id at all; `"id": null` is a malformed request rather than a notification,
// because the protocol forbids a null id.
func (r request) isNotification() bool { return len(r.ID) == 0 }

// hasNullID reports the explicit null the protocol disallows.
func (r request) hasNullID() bool { return string(r.ID) == "null" }

type response struct {
	JSONRPC string `json:"jsonrpc"`
	// Always emitted, never omitted: JSON-RPC requires an error response to
	// carry an id, and null when it could not be determined. Omitting the
	// field entirely makes the object invalid to a strict client.
	ID     json.RawMessage `json:"id"`
	Result any             `json:"result,omitempty"`
	Error  *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func result(id json.RawMessage, value any) response {
	return response{JSONRPC: "2.0", ID: id, Result: value}
}

// wellFormed reports whether a decoded message is a JSON-RPC request at all.
// Valid JSON that is not an object is an invalid request, not a parse error —
// a client deciding whether to retry needs to know which.
func (r request) wellFormed() bool { return r.JSONRPC == "2.0" && r.Method != "" }

func failure(id json.RawMessage, code int, message string, data any) response {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	return response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message, Data: data}}
}
