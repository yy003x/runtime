package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

func TestMCPClientLifecycleListPaginationAndCall(t *testing.T) {
	client, err := Start(context.Background(), Config{
		Name: "fixture", Command: os.Args[0], Args: []string{"-test.run=TestMCPHelperProcess"},
		Env: append(os.Environ(), "GO_WANT_MCP_HELPER=1"), Timeout: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "first" || tools[1].Name != "echo" {
		t.Fatalf("tools=%#v", tools)
	}
	result, err := client.CallTool(context.Background(), "echo", map[string]any{"text": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Content) != 1 || result.Content[0]["text"] != "hello" || result.IsError {
		t.Fatalf("result=%#v", result)
	}
}

func TestMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}
	os.Exit(runMCPHelper())
}

func runMCPHelper() int {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			return 2
		}
		if len(request.ID) == 0 {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch request.Method {
		case "initialize":
			response["result"] = map[string]any{
				"protocolVersion": ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fixture", "version": "1"},
			}
		case "tools/list":
			if fmt.Sprint(request.Params["cursor"]) == "next" {
				response["result"] = map[string]any{"tools": []any{map[string]any{
					"name": "echo", "description": "echo", "inputSchema": map[string]any{"type": "object"},
				}}}
			} else {
				response["result"] = map[string]any{
					"tools":      []any{map[string]any{"name": "first", "inputSchema": map[string]any{"type": "object"}}},
					"nextCursor": "next",
				}
			}
		case "tools/call":
			arguments, _ := request.Params["arguments"].(map[string]any)
			response["result"] = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(arguments["text"])}},
				"isError": false,
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		if err := encoder.Encode(response); err != nil {
			return 3
		}
	}
	return 0
}
