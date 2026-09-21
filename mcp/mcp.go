// Package mcp implements a minimal JSON-RPC 2.0 server for the MCP
// stdio transport (spec 2025-06-18: the client launches the server as a
// subprocess and speaks newline-delimited JSON-RPC over stdin/stdout),
// plus the WebSocket dial helper a bridge uses.
//
// Server identity is a parameter, not a constant. The lore spike found
// the original copy advertising the other app's name from its own
// binary; NewServer takes the name and version so every consumer
// identifies itself.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"

	"github.com/coder/websocket"
)

// ProtocolVersion is the MCP stdio revision this server speaks.
const ProtocolVersion = "2025-06-18"

// Conn aliases the websocket connection a bridge listens on.
type Conn = websocket.Conn

// Dial opens an outbound WebSocket.
func Dial(ctx context.Context, url string) (*Conn, error) {
	return DialHeader(ctx, url, nil)
}

// DialHeader opens the bridge's outbound WebSocket carrying headers.
//
// A credential belongs in a header, not in the URL. Query strings are
// written to access logs, kept in proxy history, and handed on in
// referrers, so a token in one is a token disclosed.
func DialHeader(ctx context.Context, url string, h http.Header) (*Conn, error) {
	var opts *websocket.DialOptions
	if len(h) > 0 {
		opts = &websocket.DialOptions{HTTPHeader: h}
	}
	c, _, err := websocket.Dial(ctx, url, opts)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Tool is one registered capability.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(args map[string]any) (any, error)
}

// Server is the stdio MCP server.
type Server struct {
	mu      sync.Mutex
	name    string
	version string
	tools   map[string]Tool
}

// NewServer creates a server that identifies itself as name/version.
func NewServer(name, version string) *Server {
	if name == "" {
		name = "roomkit-bridge"
	}
	if version == "" {
		version = "0.0.0"
	}
	return &Server{name: name, version: version, tools: map[string]Tool{}}
}

// Obj builds a JSON Schema object from a shorthand property map.
//
// A value may be a type name — "string", "number", "integer",
// "boolean", "object", "array" — which becomes {"type": name}, or an
// already-built schema, which passes through unchanged so a caller
// can add a description or an enum.
//
// The wrapping matters: a JSON Schema property value must be a schema
// object. Emitting the bare type name produces a document that reads
// correctly and is rejected by any validating client.
func Obj(props map[string]any) map[string]any {
	out := make(map[string]any, len(props))
	for name, spec := range props {
		switch v := spec.(type) {
		case string:
			out[name] = map[string]any{"type": v}
		case map[string]any:
			out[name] = v
		default:
			out[name] = map[string]any{"type": "string"}
		}
	}
	return map[string]any{
		"$schema":    "https://json-schema.org/draft/2020-12/schema",
		"type":       "object",
		"properties": out,
	}
}

// Required marks properties of an object schema as required. Names
// that the schema does not declare are ignored, so a rename cannot
// leave a required key pointing at nothing.
func Required(schema map[string]any, names ...string) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	req := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := props[n]; ok {
			req = append(req, n)
		}
	}
	if len(req) > 0 {
		schema["required"] = req
	}
	return schema
}

// Tool registers a capability.
func (s *Server) Tool(name, desc string, schema map[string]any, h func(map[string]any) (any, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[name] = Tool{Name: name, Description: desc, InputSchema: schema, Handler: h}
}

// ToolNames lists registered tools in a stable order.
func (s *Server) ToolNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ServeStdio runs the initialize, tools/list, and tools/call flow over
// the given reader and writer.
func (s *Server) ServeStdio(r io.Reader, w io.Writer) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	enc := json.NewEncoder(w)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		if req.JSONRPC != "2.0" {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			s.mu.Lock()
			name, version := s.name, s.version
			s.mu.Unlock()
			result = map[string]any{
				"protocolVersion": ProtocolVersion,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": name, "version": version},
			}
		case "notifications/initialized":
			continue
		case "tools/list":
			s.mu.Lock()
			tools := make([]map[string]any, 0, len(s.tools))
			for _, n := range s.toolNamesLocked() {
				t := s.tools[n]
				tools = append(tools, map[string]any{
					"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
				})
			}
			s.mu.Unlock()
			result = map[string]any{"tools": tools}
		case "tools/call":
			s.mu.Lock()
			tool, ok := s.tools[req.Params.Name]
			s.mu.Unlock()
			if !ok {
				writeErr(enc, req.ID, -32602, fmt.Sprintf("unknown tool: %s", req.Params.Name))
				continue
			}
			args := map[string]any{}
			if len(req.Params.Arguments) > 0 {
				json.Unmarshal(req.Params.Arguments, &args)
			}
			out, err := tool.Handler(args)
			if err != nil {
				// Tool errors are results with isError, not JSON-RPC errors.
				result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": err.Error()}},
					"isError": true,
				}
			} else {
				text, _ := json.Marshal(out)
				result = map[string]any{
					"content": []map[string]any{{"type": "text", "text": string(text)}},
				}
			}
		default:
			if len(req.ID) > 0 {
				writeErr(enc, req.ID, -32601, "method not found: "+req.Method)
			}
			continue
		}
		if len(req.ID) == 0 {
			continue // notification
		}
		enc.Encode(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(req.ID), "result": result})
	}
}

func (s *Server) toolNamesLocked() []string {
	names := make([]string, 0, len(s.tools))
	for n := range s.tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func writeErr(enc *json.Encoder, id json.RawMessage, code int, msg string) {
	enc.Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"error":   map[string]any{"code": code, "message": msg},
	})
}
