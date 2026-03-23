package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
)

// Server implements the MCP protocol over stdio, delegating to the
// memoryd daemon's HTTP API.
type Server struct {
	baseURL  string
	client   *http.Client
	readOnly bool
}

func NewServer(port int, readOnly bool) *Server {
	return &Server{
		baseURL:  fmt.Sprintf("http://127.0.0.1:%d", port),
		client:   &http.Client{},
		readOnly: readOnly,
	}
}

// Run reads JSON-RPC messages from stdin and writes responses to stdout.
func (s *Server) Run() error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 10*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		resp := s.handleMessage(line)
		if resp != nil {
			out, _ := json.Marshal(resp)
			fmt.Fprintf(os.Stdout, "%s\n", out)
		}
	}
	return scanner.Err()
}

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
	Error   any    `json:"error,omitempty"`
}

func (s *Server) handleMessage(data []byte) *jsonRPCResponse {
	var req jsonRPCRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", Error: map[string]any{
			"code": -32700, "message": "Parse error",
		}}
	}

	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.ID, req.Params)
	case "notifications/initialized":
		return nil // no response for notifications
	case "tools/list":
		return s.handleToolsList(req.ID)
	case "tools/call":
		return s.handleToolsCall(req.ID, req.Params)
	case "ping":
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}
	default:
		log.Printf("[mcp] unknown method: %s", req.Method)
		return &jsonRPCResponse{JSONRPC: "2.0", ID: req.ID, Error: map[string]any{
			"code": -32601, "message": fmt.Sprintf("Method not found: %s", req.Method),
		}}
	}
}

func (s *Server) handleInitialize(id any, params json.RawMessage) *jsonRPCResponse {
	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{
				"name":    "memoryd",
				"version": "1.0.0",
			},
		},
	}
}

func (s *Server) handleToolsList(id any) *jsonRPCResponse {
	tools := s.buildToolList()
	return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Result: map[string]any{"tools": tools}}
}

// writeTools are tools that modify state.
var writeTools = map[string]bool{
	"memory_store":  true,
	"memory_delete": true,
	"source_ingest": true,
	"source_upload": true,
	"source_remove": true,
}

func (s *Server) buildToolList() []map[string]any {
	all := allTools()
	if !s.readOnly {
		return all
	}
	var filtered []map[string]any
	for _, t := range all {
		name, _ := t["name"].(string)
		if !writeTools[name] {
			filtered = append(filtered, t)
		}
	}
	return filtered
}

func allTools() []map[string]any {
	return []map[string]any{
		{
			"name":        "memory_search",
			"description": "Search long-term memory for relevant context. ALWAYS call this at the start of every task — prior sessions likely have useful decisions, patterns, or partial work. Also search when you encounter unfamiliar code, before making architecture decisions, or when debugging.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"query"},
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Natural language query describing what you need to recall",
					},
					"database": map[string]any{
						"type":        "string",
						"description": "Optional: search only this database (use database_list to see available databases). Omit to search all.",
					},
				},
			},
		},
		{
			"name":        "memory_store",
			"description": "Store context in long-term memory. The system automatically deduplicates and filters noise, so store liberally. Good things to store: decisions and their rationale, architecture patterns, debugging insights, user preferences, completed work summaries, gotchas and workarounds.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"content"},
				"properties": map[string]any{
					"content": map[string]any{
						"type":        "string",
						"description": "The content to remember",
					},
					"source": map[string]any{
						"type":        "string",
						"description": "Source label (default: mcp)",
					},
				},
			},
		},
		{
			"name":        "memory_list",
			"description": "List stored memories, optionally filtered by a text query.",
			"inputSchema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"query": map[string]any{
						"type":        "string",
						"description": "Optional text filter",
					},
					"limit": map[string]any{
						"type":        "integer",
						"description": "Max results (default: 20)",
					},
				},
			},
		},
		{
			"name":        "memory_delete",
			"description": "Delete a specific memory by its ID.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "The memory ID to delete",
					},
				},
			},
		},
		{
			"name":        "source_ingest",
			"description": "Crawl a URL and ingest its content into long-term memory. Use this to add company wikis, documentation sites, or other web-based knowledge bases. The system automatically deduplicates against existing memories. For OAuth-protected sites, pass headers with Cookie or Authorization values from the user's browser.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"url", "name"},
				"properties": map[string]any{
					"url": map[string]any{
						"type":        "string",
						"description": "Base URL to crawl (e.g., https://wiki.company.com/docs)",
					},
					"name": map[string]any{
						"type":        "string",
						"description": "Short label for this source (e.g., company-wiki)",
					},
					"max_depth": map[string]any{
						"type":        "integer",
						"description": "Maximum crawl depth (default: 3)",
					},
					"max_pages": map[string]any{
						"type":        "integer",
						"description": "Maximum pages to crawl (default: 500)",
					},
					"headers": map[string]any{
						"type":                 "object",
						"description":          "Custom HTTP headers for authenticated crawling (e.g. {\"Cookie\": \"session=abc\", \"Authorization\": \"Bearer tok\"})",
						"additionalProperties": map[string]any{"type": "string"},
					},
				},
			},
		},
		{
			"name":        "source_list",
			"description": "List all ingested data sources and their status.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "source_remove",
			"description": "Remove an ingested source and all its associated memories.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"id"},
				"properties": map[string]any{
					"id": map[string]any{
						"type":        "string",
						"description": "The source ID to remove",
					},
				},
			},
		},
		{
			"name":        "source_upload",
			"description": "Upload files directly into long-term memory. Use this when crawling isn't practical — for local documents, code files, or content that can't be reached via URL.",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []string{"name", "files"},
				"properties": map[string]any{
					"name": map[string]any{
						"type":        "string",
						"description": "Short label for this source (e.g., project-docs)",
					},
					"files": map[string]any{
						"type":        "array",
						"description": "Array of files to upload",
						"items": map[string]any{
							"type":     "object",
							"required": []string{"filename", "content"},
							"properties": map[string]any{
								"filename": map[string]any{"type": "string", "description": "Name or relative path of the file"},
								"content":  map[string]any{"type": "string", "description": "Full text content of the file"},
							},
						},
					},
				},
			},
		},
		{
			"name":        "quality_stats",
			"description": "Show quality learning statistics. Reports how many retrieval events have been recorded and whether the system is still in learning mode.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
		{
			"name":        "database_list",
			"description": "List all connected databases and their roles. Your primary database is read-write; all others are read-only knowledge sources from other teams.",
			"inputSchema": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		},
	}
}

func (s *Server) handleToolsCall(id any, params json.RawMessage) *jsonRPCResponse {
	var call struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil {
		return &jsonRPCResponse{JSONRPC: "2.0", ID: id, Error: map[string]any{
			"code": -32602, "message": "Invalid params",
		}}
	}

	// Block write tools in read-only mode.
	if s.readOnly && writeTools[call.Name] {
		return &jsonRPCResponse{
			JSONRPC: "2.0",
			ID:      id,
			Result: map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": fmt.Sprintf("Tool %s is not available in read-only mode.", call.Name)},
				},
				"isError": true,
			},
		}
	}

	var result string
	var isError bool

	switch call.Name {
	case "memory_search":
		result, isError = s.callSearch(call.Arguments)
	case "memory_store":
		result, isError = s.callStore(call.Arguments)
	case "memory_list":
		result, isError = s.callList(call.Arguments)
	case "memory_delete":
		result, isError = s.callDelete(call.Arguments)
	case "source_ingest":
		result, isError = s.callSourceIngest(call.Arguments)
	case "source_upload":
		result, isError = s.callSourceUpload(call.Arguments)
	case "source_list":
		result, isError = s.callSourceList(call.Arguments)
	case "source_remove":
		result, isError = s.callSourceRemove(call.Arguments)
	case "quality_stats":
		result, isError = s.callQualityStats(call.Arguments)
	case "database_list":
		result, isError = s.callDatabaseList(call.Arguments)
	default:
		result = fmt.Sprintf("Unknown tool: %s", call.Name)
		isError = true
	}

	return &jsonRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result: map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": result},
			},
			"isError": isError,
		},
	}
}

func (s *Server) callSearch(args map[string]any) (string, bool) {
	query, _ := args["query"].(string)
	if query == "" {
		return "query is required", true
	}

	payload := map[string]string{"query": query}
	if db, ok := args["database"].(string); ok && db != "" {
		payload["database"] = db
	}

	body, _ := json.Marshal(payload)
	resp, err := s.client.Post(s.url("/api/search"), "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("search failed: %s", string(data)), true
	}

	var result struct {
		Context string `json:"context"`
	}
	json.Unmarshal(data, &result)
	if result.Context == "" {
		return "No relevant memories found.", false
	}
	return result.Context, false
}

func (s *Server) callStore(args map[string]any) (string, bool) {
	content, _ := args["content"].(string)
	if content == "" {
		return "content is required", true
	}
	source, _ := args["source"].(string)
	if source == "" {
		source = "mcp"
	}

	body, _ := json.Marshal(map[string]string{"content": content, "source": source})
	resp, err := s.client.Post(s.url("/api/store"), "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("store failed: %s", string(data)), true
	}

	var result struct {
		Summary string `json:"summary"`
	}
	data, _ := io.ReadAll(resp.Body)
	json.Unmarshal(data, &result)
	if result.Summary != "" {
		return result.Summary, false
	}
	return "Memory stored successfully.", false
}

func (s *Server) callList(args map[string]any) (string, bool) {
	query, _ := args["query"].(string)
	url := s.url("/api/memories")
	if query != "" {
		url += "?q=" + query
	}

	resp, err := s.client.Get(url)
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("list failed: %s", string(data)), true
	}

	var memories []map[string]any
	json.Unmarshal(data, &memories)

	limit := 20
	if l, ok := args["limit"].(float64); ok && int(l) > 0 {
		limit = int(l)
	}
	if len(memories) > limit {
		memories = memories[:limit]
	}

	if len(memories) == 0 {
		return "No memories stored.", false
	}

	out, _ := json.MarshalIndent(memories, "", "  ")
	return string(out), false
}

func (s *Server) callDelete(args map[string]any) (string, bool) {
	id, _ := args["id"].(string)
	if id == "" {
		return "id is required", true
	}

	req, _ := http.NewRequest("DELETE", s.url("/api/memories/"+id), nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("delete failed: %s", string(data)), true
	}
	return "Memory deleted.", false
}

func (s *Server) url(path string) string {
	return s.baseURL + path
}

func (s *Server) callSourceIngest(args map[string]any) (string, bool) {
	urlStr, _ := args["url"].(string)
	name, _ := args["name"].(string)
	if urlStr == "" || name == "" {
		return "url and name are required", true
	}

	payload := map[string]any{"base_url": urlStr, "name": name}
	if md, ok := args["max_depth"].(float64); ok {
		payload["max_depth"] = int(md)
	}
	if mp, ok := args["max_pages"].(float64); ok {
		payload["max_pages"] = int(mp)
	}
	if hdrs, ok := args["headers"].(map[string]any); ok && len(hdrs) > 0 {
		strHeaders := make(map[string]string, len(hdrs))
		for k, v := range hdrs {
			if sv, ok := v.(string); ok {
				strHeaders[k] = sv
			}
		}
		payload["headers"] = strHeaders
	}

	body, _ := json.Marshal(payload)
	resp, err := s.client.Post(s.url("/api/sources"), "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("ingest failed: %s", string(data)), true
	}

	var result map[string]string
	json.Unmarshal(data, &result)
	return fmt.Sprintf("Source '%s' created (id: %s). Crawl started in background.", name, result["id"]), false
}

func (s *Server) callSourceUpload(args map[string]any) (string, bool) {
	name, _ := args["name"].(string)
	if name == "" {
		return "name is required", true
	}
	filesRaw, ok := args["files"].([]any)
	if !ok || len(filesRaw) == 0 {
		return "files array is required and must not be empty", true
	}

	var files []map[string]string
	for _, f := range filesRaw {
		fm, ok := f.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := fm["filename"].(string)
		ct, _ := fm["content"].(string)
		if fn != "" && ct != "" {
			files = append(files, map[string]string{"filename": fn, "content": ct})
		}
	}
	if len(files) == 0 {
		return "no valid files provided (each needs filename and content)", true
	}

	payload := map[string]any{"name": name, "files": files}
	body, _ := json.Marshal(payload)
	resp, err := s.client.Post(s.url("/api/sources/upload"), "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("upload failed: %s", string(data)), true
	}

	var result map[string]string
	json.Unmarshal(data, &result)
	return fmt.Sprintf("Source '%s' created (id: %s). %s", name, result["id"], result["message"]), false
}

func (s *Server) callSourceList(args map[string]any) (string, bool) {
	resp, err := s.client.Get(s.url("/api/sources"))
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("list failed: %s", string(data)), true
	}

	var sources []map[string]any
	json.Unmarshal(data, &sources)
	if len(sources) == 0 {
		return "No sources configured.", false
	}

	out, _ := json.MarshalIndent(sources, "", "  ")
	return string(out), false
}

func (s *Server) callSourceRemove(args map[string]any) (string, bool) {
	id, _ := args["id"].(string)
	if id == "" {
		return "id is required", true
	}

	req, _ := http.NewRequest("DELETE", s.url("/api/sources/"+id), nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Sprintf("remove failed: %s", string(data)), true
	}
	return "Source removed and all associated memories deleted.", false
}

func (s *Server) callQualityStats(args map[string]any) (string, bool) {
	resp, err := s.client.Get(s.url("/api/quality"))
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("quality stats failed: %s", string(data)), true
	}

	var stats map[string]any
	json.Unmarshal(data, &stats)
	out, _ := json.MarshalIndent(stats, "", "  ")
	return string(out), false
}

func (s *Server) callDatabaseList(args map[string]any) (string, bool) {
	resp, err := s.client.Get(s.url("/api/databases"))
	if err != nil {
		return fmt.Sprintf("daemon not reachable: %v (is memoryd running?)", err), true
	}
	defer resp.Body.Close()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Sprintf("database list failed: %s", string(data)), true
	}

	var dbs []map[string]any
	json.Unmarshal(data, &dbs)
	if len(dbs) == 0 {
		return "No databases configured.", false
	}

	out, _ := json.MarshalIndent(dbs, "", "  ")
	return string(out), false
}
