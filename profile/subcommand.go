package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/internal/profileid"
	"github.com/yy003x/runtime/internal/strictjson"
)

type Subcommand struct {
	Profile string `json:"profile"`
}

type SubcommandCatalog struct {
	values map[string]Subcommand
}

func LoadSubcommands(
	directory string,
	profiles *Catalog,
	reservedIDs ...string,
) (*SubcommandCatalog, error) {
	if profiles == nil {
		return nil, fmt.Errorf("profile catalog is required")
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect subcommand directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("subcommand path must be a directory, not a symlink")
	}
	files, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read subcommand directory: %w", err)
	}
	sort.Slice(files, func(left, right int) bool {
		return files[left].Name() < files[right].Name()
	})
	reserved := make(map[string]struct{}, len(reservedIDs))
	for _, id := range reservedIDs {
		reserved[id] = struct{}{}
	}
	values := make(map[string]Subcommand, len(files))
	for _, file := range files {
		name := file.Name()
		if file.IsDir() || file.Type()&os.ModeSymlink != 0 ||
			filepath.Ext(name) != ".json" {
			return nil, fmt.Errorf(
				"subcommand directory contains unsupported entry %q", name,
			)
		}
		id := strings.TrimSuffix(name, ".json")
		if err := profileid.Validate(id); err != nil {
			return nil, fmt.Errorf("subcommand file %q: %w", name, err)
		}
		if _, exists := reserved[id]; exists {
			return nil, fmt.Errorf("subcommand %q conflicts with a fixed namespace", id)
		}
		var value Subcommand
		path := filepath.Join(directory, name)
		if err := strictjson.ReadRegularFile(path, maxProfileBytes, &value); err != nil {
			return nil, err
		}
		if err := profileid.Validate(value.Profile); err != nil {
			return nil, fmt.Errorf("%s: profile: %w", path, err)
		}
		entry, exists := profiles.Resolve(value.Profile)
		if !exists {
			return nil, fmt.Errorf(
				"%s: referenced profile %q does not exist", path, value.Profile,
			)
		}
		if entry.Kind != KindCommand || entry.Command == nil {
			return nil, fmt.Errorf(
				"%s: shortcut profile %q must be type=cli",
				path, value.Profile,
			)
		}
		if entry.Command.Transport != command.TransportTTY {
			return nil, fmt.Errorf(
				"%s: shortcut profile %q must use transport=tty",
				path, value.Profile,
			)
		}
		values[id] = value
	}
	return &SubcommandCatalog{values: values}, nil
}

func (catalog *SubcommandCatalog) Get(id string) (Subcommand, bool) {
	if catalog == nil {
		return Subcommand{}, false
	}
	value, exists := catalog.values[id]
	return value, exists
}

func (catalog *SubcommandCatalog) IDs() []string {
	if catalog == nil {
		return nil
	}
	values := make([]string, 0, len(catalog.values))
	for id := range catalog.values {
		values = append(values, id)
	}
	sort.Strings(values)
	return values
}
