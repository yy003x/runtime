package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/capability"
	"agent-runtime/internal/cli/config"
)

func newCapabilityRegistry(cfg *config.Config, sessionID string) (*capability.Registry, error) {
	memoryFile, candidateFile := cfg.Paths.MemoryFile, cfg.Paths.MemoryCandidatesFile
	if sessionID != "" {
		manager := agentrun.NewSessionManager(agentrun.New(cfg.Home))
		if _, err := manager.Store().Get(sessionID); err != nil {
			return nil, err
		}
		memoryFile, candidateFile = manager.MemoryPaths(sessionID)
	}
	return capability.NewRegistry(capability.RegistryConfig{
		SkillsDir: cfg.Paths.SkillsDir, ToolsDir: cfg.Paths.ToolsDir,
		MemoryFile: memoryFile, MemoryCandidatesFile: candidateFile,
	}), nil
}

func runToolNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: tool list|show|call")
	}
	registry, err := newCapabilityRegistry(cfg, "")
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("tool list does not accept arguments")
		}
		return printJSON(map[string]any{"tools": registry.Tools.Schemas(), "doctor": registry.Tools.Doctor()})
	case "show":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("tool show requires a tool name")
		}
		tool, err := registry.Tools.Get(args[1])
		if err != nil {
			return err
		}
		return printJSON(tool)
	case "call":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("tool call requires a tool name")
		}
		arguments := map[string]any{}
		capabilities, forbidden := []string{}, []string{}
		for index := 2; index < len(args); index++ {
			name := args[index]
			index++
			if index >= len(args) {
				return fmt.Errorf("%s requires value", name)
			}
			switch name {
			case "--args":
				if err := json.Unmarshal([]byte(args[index]), &arguments); err != nil {
					return fmt.Errorf("parse --args: %w", err)
				}
			case "--capability":
				capabilities = append(capabilities, args[index])
			case "--forbidden-action":
				forbidden = append(forbidden, args[index])
			default:
				return fmt.Errorf("unknown tool call option: %s", name)
			}
		}
		output, err := registry.Tools.Call(args[1], arguments, capabilities, forbidden)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "output": output})
	default:
		return fmt.Errorf("unknown tool action: %s", args[0])
	}
}

func runSkillNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: skill list|show|run")
	}
	registry, err := newCapabilityRegistry(cfg, "")
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("skill list does not accept arguments")
		}
		return printJSON(map[string]any{"skills": registry.Skills.List(), "doctor": registry.Skills.Doctor()})
	case "show":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("skill show requires a skill name")
		}
		skill, err := registry.Skills.Get(args[1])
		if err != nil {
			return err
		}
		return printJSON(skill)
	case "run":
		return runSkillByName(cfg, registry, args[1:])
	default:
		return fmt.Errorf("unknown skill action: %s", args[0])
	}
}

func runSkillByName(cfg *config.Config, registry *capability.Registry, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("skill run requires a skill name")
	}
	skill, err := registry.Skills.Get(args[0])
	if err != nil {
		return err
	}
	options := struct {
		input, inputFile, query string
		variables               map[string]any
		rawArgs                 []string
	}{variables: map[string]any{}}
	for index := 1; index < len(args); index++ {
		name := args[index]
		if name == "--" {
			options.rawArgs = append(options.rawArgs, args[index+1:]...)
			break
		}
		index++
		if index >= len(args) {
			return fmt.Errorf("%s requires value", name)
		}
		value := args[index]
		switch name {
		case "--input":
			options.input = value
		case "--input-file":
			options.inputFile = value
		case "--query":
			options.query = value
		case "--vars":
			if err := json.Unmarshal([]byte(value), &options.variables); err != nil {
				return fmt.Errorf("parse --vars: %w", err)
			}
		default:
			return fmt.Errorf("unknown skill run option: %s", name)
		}
	}
	if options.input != "" && options.inputFile != "" {
		return fmt.Errorf("--input and --input-file are mutually exclusive")
	}
	if options.inputFile != "" {
		data, readErr := os.ReadFile(options.inputFile)
		if readErr != nil {
			return readErr
		}
		options.input = string(data)
	}
	prompt, err := skill.Render(options.input, options.query, options.variables)
	if err != nil {
		return err
	}
	if strings.TrimSpace(skill.DefaultProfile) == "" {
		return fmt.Errorf("skill %s has no default_profile", skill.Name)
	}
	profile, ok := resolveProfile(cfg.Home, skill.DefaultProfile)
	if !ok {
		return fmt.Errorf("skill %s references unknown profile %q", skill.Name, skill.DefaultProfile)
	}
	code, err := executePromptProfile(cfg, profile, prompt, options.rawArgs)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("profile %s exited with code %d", profile.ID, code)
	}
	return nil
}

func runMemoryNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: memory list|recall|add|remove|promote")
	}
	sessionID := optionValue(args[1:], "--session-id")
	registry, err := newCapabilityRegistry(cfg, sessionID)
	if err != nil {
		return err
	}
	memory, err := registry.Memory()
	if err != nil {
		return err
	}
	candidates, err := registry.MemoryCandidates()
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if err := validateValueOptions(args[1:], map[string]bool{"--session-id": true, "--state": true}); err != nil {
			return err
		}
		state := optionDefault(args[1:], "--state", "all")
		if state != "all" && state != "working" && state != "candidate" {
			return fmt.Errorf("--state must be all|working|candidate")
		}
		result := map[string]any{"ok": true}
		if state == "all" || state == "working" {
			result["working"] = memory.Items()
		}
		if state == "all" || state == "candidate" {
			result["candidates"] = candidates.Items()
			result["promotion_required"] = true
		}
		return printJSON(result)
	case "recall":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("memory recall requires a query")
		}
		if err := validateValueOptions(args[2:], map[string]bool{"--session-id": true, "--type": true, "--top-k": true}); err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "items": memory.Recall(args[1], optionValue(args[2:], "--type"), intOption(args[2:], "--top-k", 5))})
	case "add":
		if len(args) < 3 || strings.HasPrefix(args[1], "-") || strings.HasPrefix(args[2], "-") {
			return fmt.Errorf("usage: memory add <id> <content>")
		}
		if err := validateValueOptions(args[3:], map[string]bool{"--session-id": true, "--type": true, "--source": true, "--scope": true}); err != nil {
			return err
		}
		scope := optionValue(args[3:], "--scope")
		if scope == "" {
			scope = "global"
			if sessionID != "" {
				scope = "session"
			}
		}
		item := capability.MemoryItem{ID: args[1], Type: optionDefault(args[3:], "--type", "fact"), Content: args[2],
			Source: optionValue(args[3:], "--source"), SessionID: sessionID, Scope: scope, Status: "working"}
		if err := memory.Write([]capability.MemoryItem{item}); err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "item": item.ID})
	case "remove":
		ids, state, parseErr := parseMemoryIDs(args[1:], false)
		if parseErr != nil {
			return parseErr
		}
		if state == "" {
			state = "working"
		}
		if state != "working" && state != "candidate" && state != "all" {
			return fmt.Errorf("--state must be working|candidate|all")
		}
		if state == "working" || state == "all" {
			if err := memory.Forget(ids); err != nil {
				return err
			}
		}
		if state == "candidate" || state == "all" {
			if err := candidates.Forget(ids); err != nil {
				return err
			}
		}
		return printJSON(map[string]any{"ok": true, "removed": ids, "state": state})
	case "promote":
		ids, _, parseErr := parseMemoryIDs(args[1:], true)
		if parseErr != nil {
			return parseErr
		}
		requested := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			requested[id] = struct{}{}
		}
		promoted := make([]capability.MemoryItem, 0, len(ids))
		for _, item := range candidates.Items() {
			if _, ok := requested[item.ID]; !ok {
				continue
			}
			now := time.Now().UTC()
			item.PromotedAt, item.Status = &now, "working"
			promoted = append(promoted, item)
		}
		if len(promoted) != len(requested) {
			return fmt.Errorf("one or more memory candidates were not found")
		}
		if err := memory.Write(promoted); err != nil {
			return err
		}
		if err := candidates.Forget(ids); err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "promoted": ids})
	default:
		return fmt.Errorf("unknown memory action: %s", args[0])
	}
}

func parseMemoryIDs(args []string, promote bool) ([]string, string, error) {
	ids, state := []string{}, ""
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--session-id":
			index++
			if index >= len(args) {
				return nil, "", fmt.Errorf("--session-id requires value")
			}
		case "--state":
			if promote {
				return nil, "", fmt.Errorf("unknown memory promote option: --state")
			}
			index++
			if index >= len(args) {
				return nil, "", fmt.Errorf("--state requires value")
			}
			state = args[index]
		default:
			if strings.HasPrefix(args[index], "-") {
				return nil, "", fmt.Errorf("unknown memory option: %s", args[index])
			}
			ids = append(ids, args[index])
		}
	}
	if len(ids) == 0 {
		return nil, "", fmt.Errorf("memory IDs are required")
	}
	return ids, state, nil
}

func runLoopNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: loop run|list|show|logs|cancel")
	}
	service := agentrun.New(cfg.Home)
	switch args[0] {
	case "run":
		options, err := parseLoopOptions(args[1:])
		if err != nil {
			return err
		}
		status, err := service.LoopRun(context.Background(), options)
		_ = printJSON(status)
		return err
	case "list":
		filter := agentrun.RunFilter{RunType: "loop", Limit: 100}
		for index := 1; index < len(args); index++ {
			switch args[index] {
			case "--active":
				filter.Active = true
			case "--state", "--project", "--limit":
				name := args[index]
				index++
				if index >= len(args) {
					return fmt.Errorf("%s requires value", name)
				}
				switch name {
				case "--state":
					filter.State = args[index]
				case "--project":
					filter.ProjectID = args[index]
				case "--limit":
					filter.Limit, _ = strconv.Atoi(args[index])
					if filter.Limit <= 0 {
						return fmt.Errorf("--limit must be a positive integer")
					}
				}
			default:
				return fmt.Errorf("unknown loop list option: %s", args[index])
			}
		}
		loops, err := service.ListRuns(filter)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"loops": loops})
	case "show":
		loopID, err := parseRequiredID(args[1:], "--loop-id", nil, nil)
		if err != nil {
			return err
		}
		status, err := service.LoopStatus(loopID)
		if err != nil {
			return err
		}
		return printJSON(status)
	case "logs":
		loopID, err := parseRequiredID(args[1:], "--loop-id", map[string]bool{"--tail": true}, nil)
		if err != nil {
			return err
		}
		logs, err := service.LoopLogs(loopID, intOption(args[1:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(logs)
	case "cancel":
		loopID, err := parseRequiredID(args[1:], "--loop-id", nil, nil)
		if err != nil {
			return err
		}
		status, err := service.LoopCancel(loopID)
		_ = printJSON(status)
		return err
	default:
		return fmt.Errorf("unknown loop action: %s", args[0])
	}
}
