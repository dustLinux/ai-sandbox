package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type contentItem struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	Resource *resourceItem `json:"resource,omitempty"`
}

type resourceItem struct {
	URI      string `json:"uri"`
	MimeType string `json:"mimeType,omitempty"`
	Blob     string `json:"blob,omitempty"`
}

type toolResult struct {
	Content           []contentItem `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

func textResult(format string, args ...any) toolResult {
	return toolResult{Content: []contentItem{{Type: "text", Text: fmt.Sprintf(format, args...)}}}
}

func errorResult(err error) toolResult {
	return toolResult{
		Content: []contentItem{{Type: "text", Text: err.Error()}},
		IsError: true,
	}
}

func toolDefs() []map[string]any {
	str := map[string]any{"type": "string"}
	boolean := map[string]any{"type": "boolean"}
	integer := map[string]any{"type": "integer"}
	obj := func(props map[string]any, required ...string) map[string]any {
		m := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
		if len(required) > 0 {
			m["required"] = required
		}
		return m
	}
	return []map[string]any{
		{
			"name":        "read",
			"description": "Read a text file from the sandbox workspace. Returns utf-8 text; binary files come back base64 encoded.",
			"inputSchema": obj(map[string]any{
				"path":      str,
				"offset":    integer,
				"max_bytes": integer,
			}, "path"),
		},
		{
			"name":        "write",
			"description": "Write a text file into the sandbox workspace. Creates parent directories by default.",
			"inputSchema": obj(map[string]any{
				"path":        str,
				"content":     str,
				"create_dirs": boolean,
			}, "path", "content"),
		},
		{
			"name":        "upload",
			"description": "Hand a file from the sandbox to the caller. mode=raw returns the bytes inline as an MCP resource (default), mode=url returns a temporary download link.",
			"inputSchema": obj(map[string]any{
				"path": str,
				"mode": map[string]any{"type": "string", "enum": []string{"raw", "url"}},
			}, "path"),
		},
		{
			"name":        "delete",
			"description": "Delete a file or directory from the sandbox workspace.",
			"inputSchema": obj(map[string]any{
				"path":      str,
				"recursive": boolean,
			}, "path"),
		},
		{
			"name":        "shell",
			"description": "Run a shell command as root on the sandbox machine. Working directory defaults to the workspace.",
			"inputSchema": obj(map[string]any{
				"command":         str,
				"cwd":             str,
				"timeout_seconds": integer,
			}, "command"),
		},
		{
			"name":        "execute",
			"description": "Alias of shell: run a shell command as root on the sandbox machine.",
			"inputSchema": obj(map[string]any{
				"command":         str,
				"cwd":             str,
				"timeout_seconds": integer,
			}, "command"),
		},
		{
			"name":        "end_work",
			"description": "Finish the task: archive the workspace, delete every file and release the sandbox for the next user.",
			"inputSchema": obj(map[string]any{}),
		},
	}
}

// CallTool runs one MCP tool call.
func (s *Server) CallTool(name string, args map[string]any) toolResult {
	if s.releasing() {
		return errorResult(errors.New("sandbox is being archived and released; no further commands are accepted"))
	}
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if s.releasing() {
		return errorResult(errors.New("sandbox is being archived and released; no further commands are accepted"))
	}
	s.touch(name)
	s.beginOperation()
	defer s.endOperation()
	switch name {
	case "read":
		return s.toolRead(args)
	case "write":
		return s.toolWrite(args)
	case "upload":
		return s.toolUpload(args)
	case "delete":
		return s.toolDelete(args)
	case "shell", "execute":
		return s.toolShell(args)
	case "end_work":
		return s.toolEndWork()
	default:
		return errorResult(fmt.Errorf("unknown tool %q", name))
	}
}

func (s *Server) toolRead(args map[string]any) toolResult {
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		return errorResult(errors.New("path is required"))
	}
	offset := int64(intArg(args, "offset", 0))
	max := int64(intArg(args, "max_bytes", 0))
	if max > s.cfg.MaxRead {
		max = s.cfg.MaxRead
	}
	res, err := s.readFile(path, offset, max)
	if err != nil {
		return errorResult(err)
	}
	return toolResult{
		Content:           []contentItem{{Type: "text", Text: res.Content}},
		StructuredContent: res,
	}
}

func (s *Server) toolWrite(args map[string]any) toolResult {
	path, _ := args["path"].(string)
	content, ok := args["content"].(string)
	if strings.TrimSpace(path) == "" {
		return errorResult(errors.New("path is required"))
	}
	if !ok {
		return errorResult(errors.New("content must be a string"))
	}
	createDirs := true
	if v, ok := args["create_dirs"].(bool); ok {
		createDirs = v
	}
	res, err := s.writeFile(path, content, createDirs)
	if err != nil {
		return errorResult(err)
	}
	return toolResult{
		Content:           []contentItem{{Type: "text", Text: fmt.Sprintf("wrote %d bytes to %s", res.Bytes, res.Path)}},
		StructuredContent: res,
	}
}

func (s *Server) toolUpload(args map[string]any) toolResult {
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		return errorResult(errors.New("path is required"))
	}
	mode, _ := args["mode"].(string)
	if mode == "" {
		mode = "raw"
	}
	info, err := s.statFile(path)
	if err != nil {
		return errorResult(err)
	}
	switch mode {
	case "raw":
		if info.Bytes > s.cfg.MaxInline {
			return errorResult(fmt.Errorf("file is %s; inline upload is limited to %s — use mode=url", humanBytes(info.Bytes), humanBytes(s.cfg.MaxInline)))
		}
		full, _, err := s.resolvePath(path)
		if err != nil {
			return errorResult(err)
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return errorResult(err)
		}
		sum := sha256.Sum256(data)
		res := map[string]any{
			"path":      info.Path,
			"bytes":     info.Bytes,
			"mime_type": info.MimeType,
			"sha256":    hex.EncodeToString(sum[:]),
			"encoding":  "base64",
			"mode":      "raw",
		}
		return toolResult{
			Content: []contentItem{
				{
					Type: "resource",
					Resource: &resourceItem{
						URI:      "sandbox://" + s.cfg.SandboxID + "/" + info.Path,
						MimeType: info.MimeType,
						Blob:     base64Encode(data),
					},
				},
				{Type: "text", Text: fmt.Sprintf("%s (%s, sha256 %s)", info.Path, humanBytes(info.Bytes), hex.EncodeToString(sum[:])[:16])},
			},
			StructuredContent: res,
		}
	case "url":
		full, rel, err := s.resolvePath(path)
		if err != nil {
			return errorResult(err)
		}
		sum, err := fileSHA256(full)
		if err != nil {
			return errorResult(err)
		}
		expiry := time.Now().UTC().Add(s.cfg.DownloadTTL)
		url := s.signDownload(rel, expiry)
		res := map[string]any{
			"path":       info.Path,
			"bytes":      info.Bytes,
			"mime_type":  info.MimeType,
			"sha256":     sum,
			"url":        url,
			"expires_at": expiry.Format(time.RFC3339),
			"mode":       "url",
		}
		return toolResult{
			Content:           []contentItem{{Type: "text", Text: url}},
			StructuredContent: res,
		}
	default:
		return errorResult(errors.New("mode must be raw or url"))
	}
}

func (s *Server) toolDelete(args map[string]any) toolResult {
	path, _ := args["path"].(string)
	if strings.TrimSpace(path) == "" {
		return errorResult(errors.New("path is required"))
	}
	recursive := false
	if v, ok := args["recursive"].(bool); ok {
		recursive = v
	}
	res, err := s.deletePath(path, recursive)
	if err != nil {
		return errorResult(err)
	}
	return toolResult{
		Content:           []contentItem{{Type: "text", Text: fmt.Sprintf("deleted %s (%d entries)", res.Path, res.Removed)}},
		StructuredContent: res,
	}
}

func (s *Server) toolShell(args map[string]any) toolResult {
	command, _ := args["command"].(string)
	if strings.TrimSpace(command) == "" {
		return errorResult(errors.New("command is required"))
	}
	cwd, _ := args["cwd"].(string)
	timeout := time.Duration(intArg(args, "timeout_seconds", 0)) * time.Second
	if s.cfg.LogCommands {
		logf("shell: %s", truncate(command, 200))
	}
	res, err := s.runShell(command, cwd, timeout)
	if err != nil {
		return errorResult(err)
	}
	parts := []string{}
	if res.ExitCode != nil {
		parts = append(parts, fmt.Sprintf("exit=%d", *res.ExitCode))
	}
	if res.TimedOut {
		parts = append(parts, "timed_out")
	}
	parts = append(parts, fmt.Sprintf("%dms", res.DurationMS))
	text := strings.Join(parts, " ")
	if res.Stdout != "" {
		text += "\n" + res.Stdout
	}
	if res.Stderr != "" {
		text += "\n[stderr]\n" + res.Stderr
	}
	return toolResult{
		Content:           []contentItem{{Type: "text", Text: text}},
		StructuredContent: res,
	}
}

func (s *Server) toolEndWork() toolResult {
	st := s.StartRelease("end_work")
	res := map[string]any{
		"status":        st.Status,
		"phase":         st.Phase,
		"reason":        st.ArchiveReason,
		"archive_state": st.Phase,
		"message":       "workspace is being archived, then wiped; the sandbox will be released",
	}
	return toolResult{
		Content:           []contentItem{{Type: "text", Text: "archiving and releasing the sandbox"}},
		StructuredContent: res,
	}
}

func intArg(args map[string]any, key string, def int) int {
	switch v := args[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case json.Number:
		i, err := v.Int64()
		if err != nil {
			return def
		}
		return int(i)
	}
	return def
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
