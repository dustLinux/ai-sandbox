package sandbox

import (
	"encoding/json"
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  map[string]any  `json:"params"`
}

func rpcError(id json.RawMessage, code int, message string) map[string]any {
	var requestID any
	if len(id) > 0 {
		requestID = id
	}
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      requestID,
		"error":   map[string]any{"code": code, "message": message},
	}
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	var requestID any
	if len(id) > 0 {
		requestID = id
	}
	return map[string]any{"jsonrpc": "2.0", "id": requestID, "result": result}
}

// dispatch implements the subset of MCP that agents actually use.
func (s *Server) dispatch(method string, params map[string]any) (any, int, string, bool) {
	if params == nil {
		params = map[string]any{}
	}
	switch method {
	case "initialize":
		proto, _ := params["protocolVersion"].(string)
		switch proto {
		case "2024-11-05", "2025-03-26", "2025-06-18":
		default:
			proto = "2025-06-18"
		}
		return map[string]any{
			"protocolVersion": proto,
			"capabilities":   map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":     map[string]any{"name": "ai-sandbox", "version": s.version()},
			"instructions": "Linux sandbox. Paths are relative to /workspace. shell/execute run as root on this machine. " +
				"Call end_work when the task is finished: the workspace is archived, wiped and the sandbox released.",
		}, 0, "", false
	case "notifications/initialized", "notifications/cancelled", "notifications/progress":
		return nil, 0, "", true
	case "ping":
		return map[string]any{}, 0, "", false
	case "tools/list":
		return map[string]any{"tools": toolDefs()}, 0, "", false
	case "resources/list":
		return map[string]any{"resources": []any{}}, 0, "", false
	case "prompts/list":
		return map[string]any{"prompts": []any{}}, 0, "", false
	case "tools/call":
		name, _ := params["name"].(string)
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		res := s.CallTool(name, args)
		payload := map[string]any{"content": res.Content}
		if res.StructuredContent != nil {
			payload["structuredContent"] = res.StructuredContent
		}
		if res.IsError {
			payload["isError"] = true
		}
		return payload, 0, "", false
	default:
		return nil, -32601, "Method not found", false
	}
}

func (s *Server) version() string { return "0.1.0" }

func (s *Server) rpcResponse(req rpcRequest) (map[string]any, bool) {
	result, code, message, notify := s.dispatch(req.Method, req.Params)
	if notify || len(req.ID) == 0 || string(req.ID) == "null" {
		return nil, true
	}
	if code != 0 {
		return rpcError(req.ID, code, message), false
	}
	return rpcResult(req.ID, result), false
}
