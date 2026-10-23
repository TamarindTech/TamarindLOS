package api

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/TamarindTech/TamarindLOS/services/core/internal/auth"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/catalogue"
	"github.com/TamarindTech/TamarindLOS/services/core/internal/gateway"
)

// A minimal MCP server over Streamable HTTP (JSON responses, no server-sent stream).
// Phase 0 implements initialize, ping, tools/list and tools/call. Tools are exposed
// with underscores instead of dots (application.create -> application_create) because
// several MCP clients only accept [a-zA-Z0-9_-] in tool names.

var supportedProtocolVersions = []string{"2025-06-18", "2025-03-26"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// MCPName converts a catalogue tool name to its MCP name.
func MCPName(tool string) string { return strings.ReplaceAll(tool, ".", "_") }

func (s *Server) toolFromMCPName(name string) (*catalogue.Tool, bool) {
	for _, n := range s.G.Catalogue.Names {
		if MCPName(n) == name {
			return s.G.Catalogue.Tools[n], true
		}
	}
	return nil, false
}

func (s *Server) mcp(w http.ResponseWriter, r *http.Request, p *auth.Principal) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" {
		writeJSON(w, http.StatusBadRequest, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: -32700, Message: "parse error (batches are not supported)"}})
		return
	}
	if len(req.ID) == 0 { // notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		var params struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &params)
		version := supportedProtocolVersions[0]
		for _, v := range supportedProtocolVersions {
			if v == params.ProtocolVersion {
				version = v
			}
		}
		resp.Result = map[string]any{
			"protocolVersion": version,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "tamarind-los", "title": "Tamarind LOS", "version": Version},
			"instructions": "Tamarind is a loan origination system. Every tool call is authorized, checked against " +
				"policy and recorded. Always set _envelope.reason. Mutating tools need _envelope.idempotency_key. " +
				"Call application_next_actions to see what you may do next. A status of approval_required means a " +
				"human must approve the action; do not retry it.",
		}
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = map[string]any{"tools": s.mcpTools(p)}
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			resp.Error = &rpcError{Code: -32602, Message: "invalid params"}
			break
		}
		t, ok := s.toolFromMCPName(params.Name)
		if !ok || !s.G.Implemented(t.Name) {
			resp.Error = &rpcError{Code: -32602, Message: "unknown tool: " + params.Name}
			break
		}
		args := params.Arguments
		if args == nil {
			args = map[string]any{}
		}
		var env gateway.Envelope
		if raw, ok := args["_envelope"]; ok {
			b, _ := json.Marshal(raw)
			if err := json.Unmarshal(b, &env); err != nil {
				resp.Error = &rpcError{Code: -32602, Message: "invalid _envelope"}
				break
			}
			delete(args, "_envelope")
		}
		if env.TraceID == "" {
			env.TraceID = traceID(r)
		}
		out := s.G.Invoke(r.Context(), p, t.Name, args, env)
		text, _ := json.Marshal(out)
		resp.Result = map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(text)}},
			"structuredContent": out,
			"isError":           out.Status == "error",
		}
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	writeJSON(w, http.StatusOK, resp)
}

// mcpTools lists implemented tools the caller holds scopes for.
func (s *Server) mcpTools(p *auth.Principal) []map[string]any {
	tools := []map[string]any{}
	for _, n := range s.G.Catalogue.Names {
		t := s.G.Catalogue.Tools[n]
		if !s.G.Implemented(n) {
			continue
		}
		if ok, _ := p.HasScopes(t.Scopes); !ok {
			continue
		}
		desc := t.Summary
		if t.Description != "" {
			desc += "\n\n" + t.Description
		}
		desc += "\n\nRisk tier: " + t.RiskTier + ". Default autonomy for agents: " + t.DefaultAutonomy + "."
		tools = append(tools, map[string]any{
			"name":        MCPName(n),
			"title":       n,
			"description": desc,
			"inputSchema": withEnvelope(t.Input, t.Mutates),
			"annotations": map[string]any{
				"readOnlyHint":    !t.Mutates,
				"destructiveHint": t.RiskTier == "high",
				"idempotentHint":  t.Idempotent,
				"openWorldHint":   false,
			},
		})
	}
	return tools
}

// withEnvelope adds the optional _envelope argument to a tool's input schema.
func withEnvelope(input map[string]any, mutates bool) map[string]any {
	b, _ := json.Marshal(input)
	var schema map[string]any
	_ = json.Unmarshal(b, &schema)
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		schema["properties"] = props
	}
	envDesc := "Call metadata. reason is required for agents."
	if mutates {
		envDesc += " idempotency_key is required for this tool."
	}
	props["_envelope"] = map[string]any{
		"type":        "object",
		"description": envDesc,
		"properties": map[string]any{
			"reason":                 map[string]any{"type": "string", "description": "Why you are calling this tool (stored in the audit trail)."},
			"idempotency_key":        map[string]any{"type": "string", "description": "Unique per call; retries with the same key return the original result."},
			"expected_state_version": map[string]any{"type": "integer", "description": "Fail with STATE_CONFLICT if the application changed."},
			"dry_run":                map[string]any{"type": "boolean", "description": "Check permissions and preconditions without executing."},
			"on_behalf_of":           map[string]any{"type": "string", "description": "Principal you act for, e.g. user:officer.ana."},
		},
	}
	return schema
}
