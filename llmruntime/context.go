package llmruntime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yy003x/runtime/runtimeapi"
	"gopkg.in/yaml.v3"
)

type skillDocument struct {
	Name           string `yaml:"name"`
	Description    string `yaml:"description"`
	Entry          string `yaml:"entry"`
	PromptTemplate string `yaml:"prompt_template"`
}

func compileContext(resolver *assetResolver, request runtimeapi.Request, recalled []runtimeapi.MemoryItem) (string, []runtimeapi.Message, error) {
	sections := make([]string, 0, 1+len(request.Context.Prompts)+len(request.Context.Skills)+len(request.Context.Memory)+len(recalled))
	if value := strings.TrimSpace(request.System); value != "" {
		sections = append(sections, value)
	}
	for _, ref := range request.Context.Prompts {
		asset, err := resolver.read(ref)
		if err != nil {
			return "", nil, fmt.Errorf("load prompt: %w", err)
		}
		if asset.path != "" {
			info, _ := os.Stat(asset.path)
			if info != nil && info.IsDir() {
				return "", nil, fmt.Errorf("prompt asset must be a file")
			}
		}
		if value := strings.TrimSpace(asset.content); value != "" {
			sections = append(sections, value)
		}
	}
	for _, ref := range request.Context.Skills {
		value, err := loadSkill(resolver, ref, request.Prompt)
		if err != nil {
			return "", nil, err
		}
		sections = append(sections, "<skill>\n"+value+"\n</skill>")
	}
	for _, ref := range request.Context.Memory {
		asset, err := resolver.read(ref)
		if err != nil {
			return "", nil, fmt.Errorf("load memory: %w", err)
		}
		if asset.path != "" {
			info, _ := os.Stat(asset.path)
			if info != nil && info.IsDir() {
				return "", nil, fmt.Errorf("memory asset must be a file")
			}
		}
		if value := strings.TrimSpace(asset.content); value != "" {
			sections = append(sections, "<memory>\n"+value+"\n</memory>")
		}
	}
	for _, item := range recalled {
		if value := strings.TrimSpace(item.Content); value != "" {
			sections = append(sections, "<memory source=\""+escapeMemorySource(item.Source)+"\">\n"+value+"\n</memory>")
		}
	}
	messages := append([]runtimeapi.Message(nil), request.Messages...)
	if strings.TrimSpace(request.Prompt) != "" {
		messages = append(messages, runtimeapi.Message{Role: "user", Content: request.Prompt})
	}
	if len(messages) == 0 {
		return "", nil, fmt.Errorf("request requires prompt or messages")
	}
	for index, message := range messages {
		switch message.Role {
		case "user", "assistant", "tool":
		default:
			return "", nil, fmt.Errorf("messages[%d].role must be user|assistant|tool", index)
		}
	}
	return strings.Join(sections, "\n\n"), messages, nil
}

func escapeMemorySource(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "provider"
	}
	value = strings.ReplaceAll(value, `&`, `&amp;`)
	value = strings.ReplaceAll(value, `"`, `&quot;`)
	value = strings.ReplaceAll(value, `<`, `&lt;`)
	value = strings.ReplaceAll(value, `>`, `&gt;`)
	return value
}

func loadSkill(resolver *assetResolver, ref runtimeapi.SkillRef, input string) (string, error) {
	asset, err := resolver.read(ref.AssetRef)
	if err != nil {
		return "", fmt.Errorf("load skill %s: %w", ref.Name, err)
	}
	path := asset.path
	content := asset.content
	if path != "" {
		info, statErr := os.Stat(path)
		if statErr != nil {
			return "", statErr
		}
		if info.IsDir() {
			candidates := []string{"skill.yaml", "SKILL.md"}
			if ref.Name != "" {
				candidates = append([]string{ref.Name + ".skill.yaml"}, candidates...)
			}
			path = ""
			for _, name := range candidates {
				candidate := filepath.Join(asset.path, name)
				if candidateInfo, candidateErr := os.Stat(candidate); candidateErr == nil && candidateInfo.Mode().IsRegular() {
					path = candidate
					break
				}
			}
			if path == "" {
				return "", fmt.Errorf("skill directory contains no matching skill.yaml or SKILL.md")
			}
			content, err = resolver.readPath(path)
			if err != nil {
				return "", fmt.Errorf("read skill entry: %w", err)
			}
		}
	}
	var document skillDocument
	isYAML := strings.HasSuffix(strings.ToLower(path), ".yaml") || strings.HasSuffix(strings.ToLower(path), ".yml")
	if !isYAML && path == "" && ref.Name != "" {
		if err := yaml.Unmarshal([]byte(content), &document); err == nil &&
			(document.PromptTemplate != "" || document.Entry != "") {
			isYAML = true
		}
	}
	if isYAML {
		if err := yaml.Unmarshal([]byte(content), &document); err != nil {
			return "", fmt.Errorf("parse skill yaml: %w", err)
		}
		if ref.Name != "" && document.Name != "" && ref.Name != document.Name {
			return "", fmt.Errorf("skill name mismatch: requested %s, loaded %s", ref.Name, document.Name)
		}
		content = document.PromptTemplate
		if strings.TrimSpace(content) == "" && document.Entry != "" {
			if path == "" {
				return "", fmt.Errorf("inline skill cannot use a relative entry")
			}
			if filepath.IsAbs(document.Entry) || strings.ContainsRune(document.Entry, '\x00') {
				return "", fmt.Errorf("skill entry must be relative")
			}
			base := filepath.Dir(path)
			entry, err := filepath.EvalSymlinks(filepath.Join(base, filepath.Clean(document.Entry)))
			if err != nil {
				return "", fmt.Errorf("resolve skill entry: %w", err)
			}
			relative, err := filepath.Rel(base, entry)
			if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				return "", fmt.Errorf("skill entry must stay within skill directory")
			}
			content, err = resolver.readPath(entry)
			if err != nil {
				return "", fmt.Errorf("read skill entry: %w", err)
			}
		}
	}
	if strings.TrimSpace(content) == "" {
		return "", fmt.Errorf("skill %s is empty", ref.Name)
	}
	content = strings.ReplaceAll(content, "{{input}}", input)
	content = strings.ReplaceAll(content, "{{query}}", input)
	for key, value := range ref.Variables {
		content = strings.ReplaceAll(content, "{{"+key+"}}", fmt.Sprint(value))
	}
	return strings.TrimSpace(content), nil
}
