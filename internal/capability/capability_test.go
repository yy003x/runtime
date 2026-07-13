package capability

import (
	"os"
	"path/filepath"
	"testing"
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
