package capability

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestToolGuardrailAndExternalDescription(t *testing.T) {
	manager := NewToolManager()
	if _, err := manager.Call("require-capability", map[string]any{}, nil, nil); err == nil {
		t.Fatal("missing capability was accepted")
	}
	if _, err := manager.Call("require-capability", map[string]any{}, []string{"tool.execute"}, nil); err != nil {
		t.Fatal(err)
	}
	description, err := manager.DescribeExternal("run-agent", map[string]any{"profile": "cx"}, []string{"shell"}, nil)
	if err != nil || description.(map[string]any)["type"] != "run_agent" {
		t.Fatalf("description=%#v err=%v", description, err)
	}
}

func TestToolSchemaRejectsWrongAndExtraArguments(t *testing.T) {
	schema := map[string]any{
		"type": "object", "required": []any{"name"}, "additionalProperties": false,
		"properties": map[string]any{"name": map[string]any{"type": "string"}},
	}
	if err := validateSchema(schema, map[string]any{"name": 1}, "tool.args"); err == nil {
		t.Fatal("wrong property type was accepted")
	}
	if err := validateSchema(schema, map[string]any{"name": "ok", "extra": true}, "tool.args"); err == nil {
		t.Fatal("extra property was accepted")
	}
	if err := validateSchema(schema, map[string]any{"name": "ok"}, "tool.args"); err != nil {
		t.Fatal(err)
	}
	integerSchema := map[string]any{"type": "integer"}
	if err := validateSchema(integerSchema, float64(3), "tool.count"); err != nil {
		t.Fatalf("JSON integer was rejected: %v", err)
	}
	if err := validateSchema(integerSchema, 3.5, "tool.count"); err == nil {
		t.Fatal("fractional JSON number was accepted as integer")
	}
}

func TestSkillRouteRenderAndMemory(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "skills", "review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "name: review\ndescription: review code\nkeywords: [review, 审查]\nprompt_template: 'Review {{input}} for {{query}}'\n"
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewSkillManager()
	manager.RegisterDir(filepath.Join(root, "skills"))
	skill, ok := manager.Route("请审查代码")
	if !ok {
		t.Fatal("skill not routed")
	}
	prompt, err := skill.Render("main.go", "bugs", nil)
	if err != nil || prompt != "Review main.go for bugs" {
		t.Fatalf("prompt=%q err=%v", prompt, err)
	}
	memory, err := OpenMemory(filepath.Join(root, "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Write([]MemoryItem{{ID: "1", Type: "fact", Content: "Go runtime", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	if len(memory.Recall("runtime", "fact", 5)) != 1 {
		t.Fatal("memory recall failed")
	}
	if _, err := manager.Get("review"); err != nil {
		t.Fatal(err)
	}
	if manager.Doctor()["loaded"] != 1 {
		t.Fatalf("doctor=%#v", manager.Doctor())
	}
	if len(memory.Sources()) != 1 {
		t.Fatalf("sources=%#v", memory.Sources())
	}
	if err := memory.Forget([]string{"1"}); err != nil || len(memory.Recall("runtime", "fact", 5)) != 0 {
		t.Fatalf("forget err=%v", err)
	}
}

func TestToolManagerRegistersLocalToolAndReportsErrors(t *testing.T) {
	dir := t.TempDir()
	tool := `name: local
description: local tool
kind: external
schema:
  type: object
  required: [value]
`
	if err := os.WriteFile(filepath.Join(dir, "local.tool.yaml"), []byte(tool), 0o644); err != nil {
		t.Fatal(err)
	}
	manager := NewToolManager()
	manager.RegisterDir(dir)
	manager.RegisterDir(filepath.Join(dir, "missing"))
	if len(manager.Schemas()) != 4 {
		t.Fatalf("schemas=%#v", manager.Schemas())
	}
	if manager.Doctor()["loaded"] != 4 {
		t.Fatalf("doctor=%#v", manager.Doctor())
	}
	if _, err := manager.DescribeExternal("local", map[string]any{}, nil, nil); err == nil {
		t.Fatal("missing required argument was accepted")
	}
}

func TestRegistryLoadsSkillsToolsAndMemoryFromOneConfig(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "skills", "review")
	toolDir := filepath.Join(root, "tools")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(toolDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), []byte("name: review\ndescription: review\nprompt_template: '{{input}}'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(toolDir, "local.tool.yaml"), []byte("name: local\ndescription: local\nkind: external\nschema:\n  type: object\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	registry := NewRegistry(RegistryConfig{SkillsDir: filepath.Join(root, "skills"), ToolsDir: toolDir, MemoryFile: filepath.Join(root, "memory.json")})
	if _, err := registry.Skills.Get("review"); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Tools.Get("local"); err != nil {
		t.Fatal(err)
	}
	memory, err := registry.Memory()
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Write([]MemoryItem{{ID: "fact", Type: "fact", Content: "registry"}}); err != nil {
		t.Fatal(err)
	}
	if len(memory.Recall("registry", "", 1)) != 1 {
		t.Fatal("registry memory was not loaded")
	}
}

func TestWorkspaceManagerIsolatesRunPaths(t *testing.T) {
	manager, err := NewWorkspaceManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := manager.RunWorkspace("run-1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Path("..", "outside"); err == nil {
		t.Fatal("workspace path traversal was accepted")
	}
	if err := manager.GC("run-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("workspace still exists: %v", err)
	}
}

func TestMemoryConcurrentWritersDoNotLoseItems(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	left, err := OpenMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	right, err := OpenMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	const count = 20
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		for side, memory := range []*Memory{left, right} {
			wait.Add(1)
			go func(side, index int, memory *Memory) {
				defer wait.Done()
				id := strconv.Itoa(side) + "-" + strconv.Itoa(index)
				if err := memory.Write([]MemoryItem{{ID: id, Type: "fact", Content: "shared", Source: "test"}}); err != nil {
					t.Errorf("Write: %v", err)
				}
			}(side, index, memory)
		}
	}
	wait.Wait()
	reopened, err := OpenMemory(path)
	if err != nil {
		t.Fatal(err)
	}
	if items := reopened.Recall("shared", "fact", count*2); len(items) != count*2 {
		t.Fatalf("items=%d, want %d", len(items), count*2)
	}
}

func TestMemoryRecallSkipsExpiredItems(t *testing.T) {
	memory, err := OpenMemory(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	expired := time.Now().UTC().Add(-time.Minute)
	active := time.Now().UTC().Add(time.Minute)
	if err := memory.Write([]MemoryItem{
		{ID: "expired", Type: "fact", Content: "runtime context", ExpiresAt: &expired},
		{ID: "active", Type: "fact", Content: "runtime context", ExpiresAt: &active},
	}); err != nil {
		t.Fatal(err)
	}
	items := memory.Recall("runtime", "fact", 5)
	if len(items) != 1 || items[0].ID != "active" {
		t.Fatalf("items=%#v", items)
	}
}
