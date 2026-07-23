package installbundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"

	"agent-runtime/internal/provider"
	"gopkg.in/yaml.v3"
)

// MigrationResult reports profile files changed by an explicit schema
// migration. Runtime config loading stays strict and performs no writes.
type MigrationResult struct {
	Changed []string `json:"changed_configs"`
}

// HomeMigrationResult reports explicit profile-schema migrations and legacy
// resources copied from configs/ into resources/. Legacy source files are
// intentionally preserved and existing resource files are never overwritten.
type HomeMigrationResult struct {
	ChangedConfigs  []string `json:"changed_configs"`
	CopiedResources []string `json:"copied_resources"`
}

var legacyResourceDirectories = []string{"personas", "skills", "tools", "schema"}

// MigrateHome applies all explicit runtime-home migrations. Profile documents
// remain in configs/, while non-config assets are copied to resources/.
func MigrateHome(configDir, resourcesDir string) (HomeMigrationResult, error) {
	resourceSources := make([]string, 0, len(legacyResourceDirectories))
	for _, name := range legacyResourceDirectories {
		source := filepath.Join(configDir, name)
		if _, err := os.Lstat(source); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return HomeMigrationResult{}, fmt.Errorf("stat legacy resource %s: %w", source, err)
		}
		if err := preflightSync(source, filepath.Join(resourcesDir, name)); err != nil {
			return HomeMigrationResult{}, fmt.Errorf("migrate legacy resource %s: %w", name, err)
		}
		resourceSources = append(resourceSources, name)
	}

	configResult, err := MigrateProfileConfigs(configDir)
	if err != nil {
		return HomeMigrationResult{}, err
	}
	result := HomeMigrationResult{
		ChangedConfigs:  configResult.Changed,
		CopiedResources: []string{},
	}
	for _, name := range resourceSources {
		syncResult, err := SyncMissing(filepath.Join(configDir, name), filepath.Join(resourcesDir, name))
		if err != nil {
			return HomeMigrationResult{}, fmt.Errorf("migrate legacy resource %s: %w", name, err)
		}
		for _, copied := range syncResult.Copied {
			result.CopiedResources = append(result.CopiedResources, filepath.ToSlash(filepath.Join(name, copied)))
		}
	}
	sort.Strings(result.CopiedResources)
	return result, nil
}

type LegacyProfileConfig struct {
	File   string   `json:"file"`
	Fields []string `json:"fields"`
}

type profileMigrationSource struct {
	Name     string
	Path     string
	ID       string
	Document map[string]any
	Mode     os.FileMode
}

type profileMigrationTarget struct {
	Name     string
	Path     string
	Document map[string]any
	Original map[string]any
	Mode     os.FileMode
}

type runtimeSettingsMigrationTarget struct {
	Path string
	Data []byte
	Mode os.FileMode
}

// MigrateProfileConfigs converts legacy profiles and runtime settings to the
// canonical schema. Embedded presets become standalone profiles, and an
// existing standalone profile always wins over a preset with the same ID.
func MigrateProfileConfigs(dir string) (MigrationResult, error) {
	sources, err := readProfileMigrationSources(dir)
	if err != nil {
		return MigrationResult{}, err
	}
	runtimeTarget, err := prepareRuntimeSettingsMigration(dir)
	if err != nil {
		return MigrationResult{}, err
	}
	explicit := make(map[string]string, len(sources))
	for _, source := range sources {
		if err := validateMigrationProfileID(source.ID); err != nil {
			return MigrationResult{}, fmt.Errorf("%s: %w", source.Path, err)
		}
		explicit[source.ID] = source.Name
	}

	targets := make(map[string]profileMigrationTarget, len(sources))
	presetOwners := make(map[string]string)
	for _, source := range sources {
		migrationInput := withoutShadowedPresets(source.Document, explicit)
		profiles, err := provider.CanonicalizeLegacyDocument(source.ID, migrationInput, source.Path)
		if err != nil {
			return MigrationResult{}, err
		}
		base, ok := profiles[source.ID]
		if !ok {
			return MigrationResult{}, fmt.Errorf("%s: migration did not produce base profile %q", source.Path, source.ID)
		}
		targets[source.Name] = profileMigrationTarget{
			Name: source.Name, Path: source.Path, Document: base, Original: source.Document, Mode: source.Mode,
		}
		profileIDs := make([]string, 0, len(profiles)-1)
		for profileID := range profiles {
			if profileID != source.ID {
				profileIDs = append(profileIDs, profileID)
			}
		}
		sort.Strings(profileIDs)
		for _, profileID := range profileIDs {
			if err := validateMigrationProfileID(profileID); err != nil {
				return MigrationResult{}, fmt.Errorf("%s: preset %q: %w", source.Path, profileID, err)
			}
			if _, exists := explicit[profileID]; exists {
				continue
			}
			name := profileID + ".json"
			targetPath := filepath.Join(dir, name)
			if _, statErr := os.Lstat(targetPath); statErr == nil {
				return MigrationResult{}, fmt.Errorf("preset profile target already exists but is not a standalone profile file: %s", targetPath)
			} else if !os.IsNotExist(statErr) {
				return MigrationResult{}, fmt.Errorf("stat preset profile target %s: %w", targetPath, statErr)
			}
			if owner, exists := presetOwners[name]; exists {
				return MigrationResult{}, fmt.Errorf("preset profile %q is defined by both %s and %s", profileID, owner, source.Name)
			}
			presetOwners[name] = source.Name
			targets[name] = profileMigrationTarget{
				Name: name, Path: targetPath, Document: profiles[profileID], Mode: source.Mode,
			}
		}
	}

	names := make([]string, 0, len(targets))
	for name := range targets {
		names = append(names, name)
	}
	if runtimeTarget != nil {
		names = append(names, "runtime.yaml")
	}
	sort.Strings(names)
	result := MigrationResult{Changed: []string{}}
	for _, name := range names {
		if name == "runtime.yaml" && runtimeTarget != nil {
			if err := writeFileAtomic(runtimeTarget.Path, runtimeTarget.Data, runtimeTarget.Mode.Perm()); err != nil {
				return MigrationResult{}, fmt.Errorf("write runtime settings %s: %w", runtimeTarget.Path, err)
			}
			result.Changed = append(result.Changed, name)
			continue
		}
		target := targets[name]
		if target.Original != nil && reflect.DeepEqual(target.Original, target.Document) {
			continue
		}
		data, err := encodeProfileDocument(target.Document)
		if err != nil {
			return MigrationResult{}, fmt.Errorf("encode profile config %s: %w", target.Path, err)
		}
		if err := writeFileAtomic(target.Path, data, target.Mode.Perm()); err != nil {
			return MigrationResult{}, fmt.Errorf("write profile config %s: %w", target.Path, err)
		}
		result.Changed = append(result.Changed, name)
	}
	return result, nil
}

func withoutShadowedPresets(document map[string]any, explicit map[string]string) map[string]any {
	presets, ok := document["presets"].(map[string]any)
	if !ok {
		return document
	}
	filtered := make(map[string]any, len(presets))
	for presetID, value := range presets {
		if _, exists := explicit[presetID]; !exists {
			filtered[presetID] = value
		}
	}
	if len(filtered) == len(presets) {
		return document
	}
	result := make(map[string]any, len(document))
	for key, value := range document {
		result[key] = value
	}
	if len(filtered) == 0 {
		delete(result, "presets")
	} else {
		result["presets"] = filtered
	}
	return result
}

// ScanProfileMigrations reports legacy fields without changing configuration.
// It is used by doctor when strict loading fails before migration can run.
func ScanProfileMigrations(dir string) ([]LegacyProfileConfig, error) {
	sources, err := readProfileMigrationSources(dir)
	if err != nil {
		return nil, err
	}
	result := make([]LegacyProfileConfig, 0)
	for _, source := range sources {
		fields := findLegacyProfileFields(source.Document, "")
		if len(fields) == 0 {
			continue
		}
		sort.Strings(fields)
		result = append(result, LegacyProfileConfig{File: source.Name, Fields: fields})
	}
	runtimeFields, err := scanRuntimeSettingsMigrations(dir)
	if err != nil {
		return nil, err
	}
	if len(runtimeFields) > 0 {
		result = append(result, LegacyProfileConfig{File: "runtime.yaml", Fields: runtimeFields})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].File < result[j].File })
	return result, nil
}

func prepareRuntimeSettingsMigration(dir string) (*runtimeSettingsMigrationTarget, error) {
	path := filepath.Join(dir, "runtime.yaml")
	data, mode, exists, err := readRuntimeSettingsNode(path)
	if err != nil || !exists {
		return nil, err
	}
	node, changed, err := removeLegacyRuntimeSettings(data, path)
	if err != nil || !changed {
		return nil, err
	}
	var output bytes.Buffer
	encoder := yaml.NewEncoder(&output)
	encoder.SetIndent(2)
	if err := encoder.Encode(node); err != nil {
		return nil, fmt.Errorf("encode runtime settings %s: %w", path, err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode runtime settings %s: %w", path, err)
	}
	return &runtimeSettingsMigrationTarget{Path: path, Data: output.Bytes(), Mode: mode}, nil
}

func scanRuntimeSettingsMigrations(dir string) ([]string, error) {
	path := filepath.Join(dir, "runtime.yaml")
	data, _, exists, err := readRuntimeSettingsNode(path)
	if err != nil || !exists {
		return nil, err
	}
	node, _, err := decodeRuntimeSettingsNode(data, path)
	if err != nil {
		return nil, err
	}
	fields := make([]string, 0, 2)
	for _, field := range legacyRuntimeSettingsFields() {
		if yamlMappingHasKey(node, field) {
			fields = append(fields, field)
		}
	}
	return fields, nil
}

func readRuntimeSettingsNode(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, 0, false, nil
		}
		return nil, 0, false, fmt.Errorf("stat runtime settings %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, 0, false, fmt.Errorf("runtime settings must be a regular file: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read runtime settings %s: %w", path, err)
	}
	return data, info.Mode(), true, nil
}

func removeLegacyRuntimeSettings(data []byte, path string) (*yaml.Node, bool, error) {
	node, _, err := decodeRuntimeSettingsNode(data, path)
	if err != nil {
		return nil, false, err
	}
	changed := false
	for _, field := range legacyRuntimeSettingsFields() {
		if yamlMappingDelete(node, field) {
			changed = true
		}
	}
	return node, changed, nil
}

func decodeRuntimeSettingsNode(data []byte, path string) (*yaml.Node, bool, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var node yaml.Node
	if err := decoder.Decode(&node); err != nil {
		if err == io.EOF {
			return &node, false, nil
		}
		return nil, false, fmt.Errorf("decode runtime settings %s: %w", path, err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, false, fmt.Errorf("decode runtime settings %s: multiple YAML documents", path)
		}
		return nil, false, fmt.Errorf("decode runtime settings %s: %w", path, err)
	}
	return &node, true, nil
}

func yamlMappingNode(node *yaml.Node) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.DocumentNode && len(node.Content) == 1 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil
	}
	return node
}

func yamlMappingHasKey(node *yaml.Node, key string) bool {
	mapping := yamlMappingNode(node)
	if mapping == nil {
		return false
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			return true
		}
	}
	return false
}

func yamlMappingDelete(node *yaml.Node, key string) bool {
	mapping := yamlMappingNode(node)
	if mapping == nil {
		return false
	}
	for index := 0; index+1 < len(mapping.Content); index += 2 {
		if mapping.Content[index].Value == key {
			mapping.Content = append(mapping.Content[:index], mapping.Content[index+2:]...)
			return true
		}
	}
	return false
}

func legacyRuntimeSettingsFields() []string {
	return []string{"provider_config_dir", "runs_dir"}
}

func readProfileMigrationSources(dir string) ([]profileMigrationSource, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read profile config directory %s: %w", dir, err)
	}
	sources := make([]profileMigrationSource, 0, len(entries))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			return nil, fmt.Errorf("profile config is not a regular file: %s", path)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("profile config is a symlink: %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat profile config %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("profile config is not a regular file: %s", path)
		}
		document, mode, err := readProfileMigrationDocument(path)
		if err != nil {
			return nil, err
		}
		sources = append(sources, profileMigrationSource{
			Name: entry.Name(), Path: path, ID: strings.TrimSuffix(entry.Name(), ".json"), Document: document, Mode: mode,
		})
	}
	return sources, nil
}

func readProfileMigrationDocument(path string) (map[string]any, os.FileMode, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, fmt.Errorf("read profile config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, 0, fmt.Errorf("decode profile config %s: %w", path, err)
	}
	if document == nil {
		return nil, 0, fmt.Errorf("decode profile config %s: profile must be a JSON object", path)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, 0, fmt.Errorf("decode profile config %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0, fmt.Errorf("stat profile config %s: %w", path, err)
	}
	return document, info.Mode(), nil
}

func validateMigrationProfileID(id string) error {
	if strings.TrimSpace(id) == "" || id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\\`) {
		return fmt.Errorf("unsafe profile id %q", id)
	}
	if strings.HasSuffix(id, ".local") {
		return fmt.Errorf("profile id %q would create unsupported .local.json config", id)
	}
	if _, reserved := provider.ReservedCommands[id]; reserved {
		return fmt.Errorf("profile id %q conflicts with a built-in command", id)
	}
	return nil
}

func encodeProfileDocument(document map[string]any) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func findLegacyProfileFields(document map[string]any, prefix string) []string {
	found := make([]string, 0, 8)
	for _, field := range []string{"id", "label", "type", "cli", "api", "native", "depends", "execution"} {
		if _, exists := document[field]; exists {
			found = append(found, prefix+field)
		}
	}
	for _, field := range []string{
		"aliases", "driver", "binary", "managed_args", "env_passthrough", "env_unset", "executor", "tmux",
		"prompt_delivery", "prompt_args", "override_policy", "auth", "headers", "stream", "mock", "runtime",
		"result_contract", "api_key_env",
	} {
		if _, exists := document[field]; exists {
			found = append(found, prefix+field)
		}
	}
	if cli, ok := document["cli"].(map[string]any); ok {
		if _, exists := cli["command"].(map[string]any); exists {
			found = append(found, prefix+"cli.command")
		}
		for _, field := range []string{"driver", "binary", "managed_args", "env_passthrough", "env_unset", "executor", "tmux", "prompt_delivery", "prompt_args", "override_policy"} {
			if _, exists := cli[field]; exists {
				found = append(found, prefix+"cli."+field)
			}
		}
		if _, exists := cli["runtime"]; exists {
			found = append(found, prefix+"cli.runtime")
		}
		if tmux, ok := cli["tmux"].(map[string]any); ok {
			for _, field := range legacyTmuxFields() {
				if _, exists := tmux[field]; exists {
					found = append(found, prefix+"cli.tmux."+field)
				}
			}
		}
	}
	if api, ok := document["api"].(map[string]any); ok {
		for _, field := range []string{"auth", "headers", "stream", "mock", "runtime", "override_policy", "result_contract", "api_key_env"} {
			if _, exists := api[field]; exists {
				found = append(found, prefix+"api."+field)
			}
		}
	}
	if prefix != "" {
		return found
	}
	presets, ok := document["presets"].(map[string]any)
	if !ok {
		return found
	}
	found = append(found, "presets")
	presetIDs := make([]string, 0, len(presets))
	for presetID := range presets {
		presetIDs = append(presetIDs, presetID)
	}
	sort.Strings(presetIDs)
	for _, presetID := range presetIDs {
		preset, ok := presets[presetID].(map[string]any)
		if !ok {
			continue
		}
		found = append(found, findLegacyProfileFields(preset, "presets."+presetID+".")...)
	}
	return found
}

func legacyTmuxFields() []string {
	return []string{
		"tmux_input_mode", "ready_timeout_seconds", "prompt_idle_timeout_seconds",
		"prompt_ready_settle_seconds", "prompt_ready_settle_fast_seconds", "session_wait_ready",
		"silence_threshold_seconds", "output_rate_window_seconds", "tail_bytes", "auto_trust_cwd",
	}
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".migrate-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}
