// Package profile owns the unified user-facing Profile catalog. A Profile file
// selects either the CLI or API execution domain while both domains remain
// separate behind the catalog.
package profile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yy003x/runtime/internal/domain/profileid"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/model"
)

const maxProfileBytes int64 = 1 << 20

type Kind string

const (
	KindCommand Kind = "cli"
	KindModel   Kind = "api"
)

var ReservedIDs = []string{"list", "show", "check"}

type Entry struct {
	ID      string           `json:"id"`
	Kind    Kind             `json:"kind"`
	Command *command.Profile `json:"command,omitempty"`
	Model   *model.Profile   `json:"model,omitempty"`
}

type Catalog struct {
	commands *command.Catalog
	models   *model.Catalog
	entries  map[string]Kind
}

type cliConfig struct {
	Type string `json:"type"`
	command.Profile
}

type apiConfig struct {
	Type string `json:"type"`
	model.Profile
}

func Load(configDir string, reservedIDs ...string) (*Catalog, error) {
	info, err := os.Lstat(configDir)
	if err != nil {
		return nil, fmt.Errorf("inspect profile config directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("profile config path must be a directory, not a symlink")
	}
	files, err := os.ReadDir(configDir)
	if err != nil {
		return nil, fmt.Errorf("read profile config directory: %w", err)
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].Name() < files[right].Name()
	})
	commands := make(map[string]command.Profile)
	models := make(map[string]model.Profile)
	allReservedIDs := append([]string(nil), ReservedIDs...)
	allReservedIDs = append(allReservedIDs, reservedIDs...)
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || file.Type()&os.ModeSymlink != 0 ||
			filepath.Ext(name) != ".json" {
			return nil, fmt.Errorf(
				"profile config directory contains unsupported entry %q", name,
			)
		}
		id := strings.TrimSuffix(name, ".json")
		if err := profileid.Validate(id); err != nil {
			return nil, fmt.Errorf("profile config file %q: %w", name, err)
		}
		if contains(allReservedIDs, id) {
			return nil, fmt.Errorf("profile %q conflicts with a reserved profile ID", id)
		}
		kind, cliProfile, apiProfile, err := loadFile(filepath.Join(configDir, name))
		if err != nil {
			return nil, err
		}
		switch kind {
		case KindCommand:
			commands[id] = cliProfile
		case KindModel:
			models[id] = apiProfile
		}
	}
	commandCatalog, err := command.NewCatalog(commands, allReservedIDs...)
	if err != nil {
		return nil, err
	}
	modelCatalog, err := model.NewCatalog(models, allReservedIDs...)
	if err != nil {
		return nil, err
	}
	return NewCatalog(commandCatalog, modelCatalog)
}

func loadFile(path string) (Kind, command.Profile, model.Profile, error) {
	raw := make(map[string]json.RawMessage)
	if err := strictjson.ReadRegularFile(path, maxProfileBytes, &raw); err != nil {
		return "", command.Profile{}, model.Profile{}, err
	}
	typeValue, exists := raw["type"]
	if !exists {
		return "", command.Profile{}, model.Profile{}, fmt.Errorf("%s: type is required", path)
	}
	var profileType string
	if err := json.Unmarshal(typeValue, &profileType); err != nil {
		return "", command.Profile{}, model.Profile{}, fmt.Errorf("%s: type must be a string", path)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return "", command.Profile{}, model.Profile{}, fmt.Errorf("%s: normalize profile: %w", path, err)
	}
	if err := strictjson.RejectNulls(data, func(parts []string) bool {
		switch Kind(profileType) {
		case KindCommand:
			return len(parts) == 2 && parts[0] == "env"
		case KindModel:
			return len(parts) == 2 &&
				(parts[0] == "parameters" &&
					(parts[1] == "max_tokens" ||
						parts[1] == "temperature" ||
						parts[1] == "top_p") ||
					parts[0] == "context" &&
						parts[1] == "summary_enabled")
		default:
			return false
		}
	}); err != nil {
		return "", command.Profile{}, model.Profile{}, fmt.Errorf(
			"%s: %w", path, err,
		)
	}
	switch Kind(profileType) {
	case KindCommand:
		var config cliConfig
		if err := strictjson.Decode(
			bytes.NewReader(data), int64(len(data)), &config,
		); err != nil {
			return "", command.Profile{}, model.Profile{}, fmt.Errorf("%s: %w", path, err)
		}
		if err := command.CheckProfile(config.Profile); err != nil {
			return "", command.Profile{}, model.Profile{}, fmt.Errorf("%s: %w", path, err)
		}
		return KindCommand, config.Profile, model.Profile{}, nil
	case KindModel:
		var config apiConfig
		if err := strictjson.Decode(
			bytes.NewReader(data), int64(len(data)), &config,
		); err != nil {
			return "", command.Profile{}, model.Profile{}, fmt.Errorf("%s: %w", path, err)
		}
		if err := config.Profile.Validate(); err != nil {
			return "", command.Profile{}, model.Profile{}, fmt.Errorf("%s: %w", path, err)
		}
		return KindModel, command.Profile{}, config.Profile, nil
	default:
		return "", command.Profile{}, model.Profile{}, fmt.Errorf(
			"%s: type must be %q or %q", path, KindCommand, KindModel,
		)
	}
}

func NewCatalog(commands *command.Catalog, models *model.Catalog) (*Catalog, error) {
	if commands == nil {
		return nil, fmt.Errorf("command catalog is required")
	}
	if models == nil {
		return nil, fmt.Errorf("model catalog is required")
	}
	entries := make(map[string]Kind, len(commands.IDs())+len(models.IDs()))
	for _, id := range commands.IDs() {
		entries[id] = KindCommand
	}
	for _, id := range models.IDs() {
		if existing, exists := entries[id]; exists {
			return nil, fmt.Errorf("profile ID %q exists in both %s and %s catalogs", id, existing, KindModel)
		}
		entries[id] = KindModel
	}
	return &Catalog{commands: commands, models: models, entries: entries}, nil
}

func (catalog *Catalog) Resolve(id string) (Entry, bool) {
	if catalog == nil {
		return Entry{}, false
	}
	switch catalog.entries[id] {
	case KindCommand:
		value, exists := catalog.commands.Get(id)
		if !exists {
			return Entry{}, false
		}
		return Entry{ID: id, Kind: KindCommand, Command: &value}, true
	case KindModel:
		value, exists := catalog.models.Get(id)
		if !exists {
			return Entry{}, false
		}
		return Entry{ID: id, Kind: KindModel, Model: &value}, true
	default:
		return Entry{}, false
	}
}

func (catalog *Catalog) Entries() []Entry {
	if catalog == nil {
		return nil
	}
	ids := make([]string, 0, len(catalog.entries))
	for id := range catalog.entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	values := make([]Entry, 0, len(ids))
	for _, id := range ids {
		value, _ := catalog.Resolve(id)
		values = append(values, value)
	}
	return values
}

func (catalog *Catalog) CommandCatalog() *command.Catalog {
	if catalog == nil {
		return nil
	}
	return catalog.commands
}

func (catalog *Catalog) ModelCatalog() *model.Catalog {
	if catalog == nil {
		return nil
	}
	return catalog.models
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
