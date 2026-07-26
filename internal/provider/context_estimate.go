package provider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/yy003x/runtime/internal/capability"
	"github.com/yy003x/runtime/internal/persona"
	nativeengine "github.com/yy003x/runtime/internal/provider/native"
)

type ContextEstimateRequest struct {
	Prompt     string
	Overrides  map[string]any
	PersonaDir string
	SkillDir   string
	ToolDir    string
	MemoryFile string
	Allowed    []string
	Forbidden  []string
}

type ContextEstimateComponent struct {
	Category        string `json:"category"`
	ID              string `json:"id"`
	EstimatedTokens int    `json:"estimated_tokens"`
	Digest          string `json:"digest"`
	Source          string `json:"source"`
}

type ContextUnknownComponent struct {
	Category string `json:"category"`
	Reason   string `json:"reason"`
	Source   string `json:"source"`
}

type StaticToolSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type StaticContextSnapshot struct {
	SystemSections []string           `json:"system_sections,omitempty"`
	ToolSchemas    []StaticToolSchema `json:"tool_schemas,omitempty"`
	Digest         string             `json:"digest"`
}

type StaticContextEstimate struct {
	Counted  []ContextEstimateComponent `json:"counted_components,omitempty"`
	Unknown  []ContextUnknownComponent  `json:"unknown_components,omitempty"`
	Snapshot StaticContextSnapshot      `json:"snapshot"`
}

func EstimateStaticContext(
	ctx context.Context,
	cfg Config,
	request ContextEstimateRequest,
) (StaticContextEstimate, error) {
	estimate := StaticContextEstimate{}
	addText := func(category, id, source, content string) {
		content = strings.TrimSpace(content)
		if content == "" {
			return
		}
		estimate.Counted = append(estimate.Counted, ContextEstimateComponent{
			Category: category, ID: id, Source: source,
			EstimatedTokens: estimateStaticTextTokens(content),
			Digest:          staticDigest([]byte(content)),
		})
		estimate.Snapshot.SystemSections = append(estimate.Snapshot.SystemSections, content)
	}
	addTool := func(category, source string, tool nativeengine.Tool) {
		encoded, _ := json.Marshal(tool)
		estimate.Counted = append(estimate.Counted, ContextEstimateComponent{
			Category: category, ID: tool.Name, Source: source,
			EstimatedTokens: estimateStaticTextTokens(string(encoded)),
			Digest:          staticDigest(encoded),
		})
		estimate.Snapshot.ToolSchemas = append(estimate.Snapshot.ToolSchemas, StaticToolSchema{
			Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters,
		})
	}

	switch cfg.Type {
	case TypeCLI:
		estimate.Unknown = append(estimate.Unknown, ContextUnknownComponent{
			Category: "provider_managed_context",
			Reason:   "external CLI provider system instructions and tools are not observable",
			Source:   cfg.ID,
		})
	case TypeAPI:
		if cfg.API != nil && cfg.API.Runtime != nil && cfg.API.Runtime.Enabled {
			runtime := cfg.API.Runtime
			addText("system_prompt", "api-runtime", "profile", runtime.SystemPrompt)
			skills, err := loadAPIRuntimeSkills(request.SkillDir, runtime, request.Prompt)
			if err != nil {
				return StaticContextEstimate{}, err
			}
			for index, skill := range skills {
				addText("skill", fmt.Sprintf("%04d", index), request.SkillDir, skill)
			}
			memory, err := loadAPIRuntimeMemory(request.MemoryFile, runtime.Memory, request.Prompt)
			if err != nil {
				return StaticContextEstimate{}, err
			}
			addText("runtime_memory", "recall", request.MemoryFile, memory)
			registry := capability.NewRegistry(capability.RegistryConfig{ToolsDir: request.ToolDir})
			for _, schema := range registry.Tools.Schemas() {
				if schema.Kind == "external" ||
					!agentActionAllowed(schema.Name, schema.Capability, request.Allowed, request.Forbidden) {
					continue
				}
				addTool("local_tool_schema", request.ToolDir, nativeengine.Tool{
					Name: schema.Name, Description: schema.Description, Parameters: schema.Schema,
				})
			}
			for _, tool := range apiMemoryToolSchemas(*runtime, request.Allowed, request.Forbidden) {
				addTool("memory_tool_schema", "runtime", tool)
			}
			for _, server := range runtime.MCPServers {
				if !mcpServerPotentiallyAllowed(server.Name, request.Allowed, request.Forbidden) {
					continue
				}
				estimate.Unknown = append(estimate.Unknown, ContextUnknownComponent{
					Category: "mcp_tool_schema",
					Reason:   "tool schema is available only after remote ListTools",
					Source:   server.Name,
				})
			}
		}
	case TypeNative:
		systemPrompt, personaID := "", ""
		if cfg.Native != nil {
			prepared, err := prepareNative(cfg, cloneStringMap(request.Overrides))
			if err != nil {
				return StaticContextEstimate{}, err
			}
			systemPrompt = strings.TrimSpace(fmt.Sprint(prepared.EffectiveOptions["system_prompt"]))
			personaID = strings.TrimSpace(fmt.Sprint(prepared.EffectiveOptions["persona"]))
		}
		source := "profile"
		if systemPrompt == "" && personaID != "" && request.PersonaDir != "" {
			loaded, err := persona.NewLoader(request.PersonaDir).Load(ctx, personaID)
			if err != nil {
				return StaticContextEstimate{}, fmt.Errorf("load native persona: %w", err)
			}
			systemPrompt = persona.RenderSystem(loaded)
			source = request.PersonaDir
		}
		addText("system_prompt", "native", source, systemPrompt)
		registry := capability.NewRegistry(capability.RegistryConfig{ToolsDir: request.ToolDir})
		for _, schema := range registry.Tools.Schemas() {
			if schema.Kind == "external" ||
				!nativeToolAllowed(schema, request.Allowed, request.Forbidden) {
				continue
			}
			addTool("local_tool_schema", request.ToolDir, nativeengine.Tool{
				Name: schema.Name, Description: schema.Description, Parameters: schema.Schema,
			})
		}
	}

	estimate.Unknown = append(estimate.Unknown, ContextUnknownComponent{
		Category: "model_tokenizer_special_tokens",
		Reason:   "runtime uses a UTF-8 heuristic instead of the provider tokenizer",
		Source:   cfg.ID,
	})
	sort.Slice(estimate.Counted, func(i, j int) bool {
		left, right := estimate.Counted[i], estimate.Counted[j]
		if left.Category != right.Category {
			return left.Category < right.Category
		}
		if left.ID != right.ID {
			return left.ID < right.ID
		}
		return left.Source < right.Source
	})
	sort.Slice(estimate.Unknown, func(i, j int) bool {
		left, right := estimate.Unknown[i], estimate.Unknown[j]
		if left.Category != right.Category {
			return left.Category < right.Category
		}
		if left.Reason != right.Reason {
			return left.Reason < right.Reason
		}
		return left.Source < right.Source
	})
	sort.Slice(estimate.Snapshot.ToolSchemas, func(i, j int) bool {
		return estimate.Snapshot.ToolSchemas[i].Name < estimate.Snapshot.ToolSchemas[j].Name
	})
	encoded, _ := json.Marshal(struct {
		Counted        []ContextEstimateComponent `json:"counted"`
		SystemSections []string                   `json:"system_sections,omitempty"`
		ToolSchemas    []StaticToolSchema         `json:"tool_schemas,omitempty"`
	}{estimate.Counted, estimate.Snapshot.SystemSections, estimate.Snapshot.ToolSchemas})
	estimate.Snapshot.Digest = staticDigest(encoded)
	return estimate, nil
}

func ValidateStaticContextSnapshot(ctx context.Context, cfg Config, request Request) error {
	if request.StaticContext == nil {
		return nil
	}
	current, err := EstimateStaticContext(ctx, cfg, ContextEstimateRequest{
		Prompt: request.Prompt, Overrides: request.Overrides,
		PersonaDir: request.PersonaDir, SkillDir: request.SkillDir,
		ToolDir: request.ToolDir, MemoryFile: request.MemoryFile,
		Allowed: request.Allowed, Forbidden: request.Forbidden,
	})
	if err != nil {
		return fmt.Errorf("context_inputs_changed: re-evaluate static context: %w", err)
	}
	if current.Snapshot.Digest != request.StaticContext.Digest {
		return fmt.Errorf(
			"context_inputs_changed: static context digest changed from %s to %s",
			request.StaticContext.Digest, current.Snapshot.Digest,
		)
	}
	return nil
}

func estimateStaticTextTokens(value string) int {
	units := 0
	for _, char := range value {
		if char <= 0x7f {
			units++
		} else {
			units += 4
		}
	}
	return (units + 3) / 4
}

func staticDigest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func snapshotTool(request Request, tool nativeengine.Tool) (nativeengine.Tool, bool) {
	if request.StaticContext == nil {
		return tool, true
	}
	for _, candidate := range request.StaticContext.ToolSchemas {
		if candidate.Name == tool.Name {
			tool.Description = candidate.Description
			tool.Parameters = candidate.Parameters
			return tool, true
		}
	}
	return nativeengine.Tool{}, false
}
