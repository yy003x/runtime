package internal

import (
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/testkit/reporoot"
)

const modulePath = "github.com/yy003x/runtime"

type listedPackage struct {
	ImportPath string
	Imports    []string
}

func TestSourceLayoutAndLayerDependencies(t *testing.T) {
	packages := listProductionPackages(t)
	violations := make([]string, 0)

	for _, current := range packages {
		relative, found := strings.CutPrefix(current.ImportPath, modulePath+"/")
		if !found {
			continue
		}
		top, _, _ := strings.Cut(relative, "/")
		if top != "cmd" && top != "pkg" && top != "internal" {
			violations = append(violations,
				current.ImportPath+" is outside cmd, pkg, or internal",
			)
		}

		for _, dependency := range current.Imports {
			if !strings.HasPrefix(dependency, modulePath+"/") {
				continue
			}
			switch {
			case inLayer(current.ImportPath, "internal/domain"):
				if !inLayer(dependency, "internal/domain") {
					violations = append(violations,
						current.ImportPath+" domain-imports "+dependency,
					)
				}
			case inLayer(current.ImportPath, "internal/infrastructure"):
				if inLayer(dependency, "internal/application") ||
					inLayer(dependency, "internal/interfaces") {
					violations = append(violations,
						current.ImportPath+" adapter-imports "+dependency,
					)
				}
			case inLayer(current.ImportPath, "internal/application"):
				if inLayer(dependency, "internal/interfaces") {
					violations = append(violations,
						current.ImportPath+" application-imports "+dependency,
					)
				}
			case inLayer(current.ImportPath, "pkg"):
				if inLayer(dependency, "internal/application") ||
					inLayer(dependency, "internal/interfaces") {
					violations = append(violations,
						current.ImportPath+" public-package-imports "+dependency,
					)
				}
			}
		}
	}

	sort.Strings(violations)
	if len(violations) != 0 {
		t.Fatalf("architecture violations:\n%s", strings.Join(violations, "\n"))
	}
}

func inLayer(importPath, layer string) bool {
	prefix := modulePath + "/" + layer
	return importPath == prefix || strings.HasPrefix(importPath, prefix+"/")
}

func listProductionPackages(t *testing.T) []listedPackage {
	t.Helper()
	goBinary := filepath.Join(runtime.GOROOT(), "bin", "go")
	command := exec.Command(
		goBinary, "-C", reporoot.Root(t), "list", "-json", "./...",
	)
	output, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("go list: %v\n%s", err, exitErr.Stderr)
		}
		t.Fatalf("go list: %v", err)
	}

	decoder := json.NewDecoder(strings.NewReader(string(output)))
	packages := make([]listedPackage, 0)
	for {
		var current listedPackage
		if err := decoder.Decode(&current); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatalf("decode go list output: %v", err)
		}
		packages = append(packages, current)
	}
	return packages
}
