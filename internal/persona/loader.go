package persona

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Loader struct {
	dir string
}

func NewLoader(dir string) *Loader {
	return &Loader{dir: dir}
}

func (l *Loader) Load(ctx context.Context, id string) (Persona, error) {
	_ = ctx

	raw, err := os.ReadFile(filepath.Join(l.dir, id+".yaml"))
	if err != nil {
		return Persona{}, fmt.Errorf("read persona %q: %w", id, err)
	}

	var p Persona
	if err := yaml.Unmarshal(raw, &p); err != nil {
		return Persona{}, fmt.Errorf("unmarshal persona %q: %w", id, err)
	}

	if p.ID == "" {
		p.ID = id
	}
	if p.Name == "" {
		p.Name = strings.Title(id)
	}

	return p, nil
}
