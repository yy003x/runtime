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

// allowedPublicModuleDependencies is the reviewed dependency surface of every
// public Runtime package. Keeping this list explicit prevents a new dependency
// on an internal adapter (or another Runtime domain) from silently weakening
// the package boundaries documented in AGENTS.md and runtime-contract.md.
var allowedPublicModuleDependencies = map[string][]string{
	modulePath + "/pkg": {},
	modulePath + "/pkg/agent": {
		modulePath + "/internal/domain/identity",
		modulePath + "/internal/infrastructure/strictjson",
		modulePath + "/pkg/contract",
		modulePath + "/pkg/model",
	},
	modulePath + "/pkg/command": {
		modulePath + "/internal/domain/profileid",
		modulePath + "/internal/infrastructure/envref",
		modulePath + "/pkg/contract",
	},
	modulePath + "/pkg/contract": {
		modulePath + "/internal/domain/profileid",
	},
	modulePath + "/pkg/model": {
		modulePath + "/internal/domain/profileid",
		modulePath + "/internal/infrastructure/envref",
		modulePath + "/pkg/contract",
		modulePath + "/pkg/provider",
	},
	modulePath + "/pkg/profile": {
		modulePath + "/internal/domain/profileid",
		modulePath + "/internal/infrastructure/strictjson",
		modulePath + "/pkg/command",
		modulePath + "/pkg/model",
	},
	modulePath + "/pkg/provider": {},
	modulePath + "/pkg/provider/anthropic": {
		modulePath + "/internal/infrastructure/providerhttp",
		modulePath + "/pkg/contract",
		modulePath + "/pkg/model",
		modulePath + "/pkg/provider",
	},
	modulePath + "/pkg/provider/openai": {
		modulePath + "/internal/infrastructure/providerhttp",
		modulePath + "/pkg/contract",
		modulePath + "/pkg/model",
		modulePath + "/pkg/provider",
	},
	modulePath + "/pkg/run": {
		modulePath + "/internal/domain/identity",
		modulePath + "/internal/infrastructure/strictjson",
		modulePath + "/pkg/agent",
		modulePath + "/pkg/contract",
		modulePath + "/pkg/model",
		modulePath + "/pkg/profile",
		modulePath + "/pkg/session",
	},
	modulePath + "/pkg/session": {
		modulePath + "/internal/domain/identity",
		modulePath + "/internal/infrastructure/executionlog",
		modulePath + "/internal/infrastructure/strictjson",
		modulePath + "/pkg/agent",
		modulePath + "/pkg/command",
		modulePath + "/pkg/contract",
		modulePath + "/pkg/model",
		modulePath + "/pkg/profile",
	},
	modulePath + "/pkg/store/sqlite": {
		modulePath + "/internal/domain/identity",
		modulePath + "/internal/infrastructure/strictjson",
		modulePath + "/pkg/contract",
		modulePath + "/pkg/run",
	},
	modulePath + "/pkg/transport/http": {
		modulePath + "/internal/domain/identity",
		modulePath + "/internal/infrastructure/strictjson",
		modulePath + "/pkg/agent",
		modulePath + "/pkg/contract",
		modulePath + "/pkg/model",
		modulePath + "/pkg/run",
		modulePath + "/pkg/session",
	},
}

var allowedConcreteAdapterConsumers = map[string][]string{
	modulePath + "/internal/infrastructure/providerhttp": {
		modulePath + "/pkg/provider/anthropic",
		modulePath + "/pkg/provider/openai",
	},
	modulePath + "/internal/infrastructure/tmux": {
		modulePath + "/internal/application/nativeconsole",
		modulePath + "/internal/application/runtimebootstrap",
		modulePath + "/internal/interfaces/cli",
	},
	modulePath + "/internal/infrastructure/toolbuiltin": {
		modulePath + "/internal/application/activation",
		modulePath + "/internal/application/runtimebootstrap",
	},
	modulePath + "/internal/infrastructure/toolmcp": {
		modulePath + "/internal/application/runtimebootstrap",
	},
	modulePath + "/pkg/provider/anthropic": {
		modulePath + "/internal/application/runtimebootstrap",
	},
	modulePath + "/pkg/provider/openai": {
		modulePath + "/internal/application/runtimebootstrap",
	},
	modulePath + "/pkg/store/sqlite": {
		modulePath + "/internal/application/activation",
		modulePath + "/internal/application/runtimebootstrap",
	},
}

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
		if inLayer(current.ImportPath, "pkg") {
			if _, declared := allowedPublicModuleDependencies[current.ImportPath]; !declared {
				violations = append(violations,
					current.ImportPath+" is not a declared public package",
				)
			}
			if strings.Contains(current.ImportPath+"/", "/internal/") {
				violations = append(violations,
					current.ImportPath+" is private implementation under pkg",
				)
			}
		}

		for _, dependency := range current.Imports {
			if !strings.HasPrefix(dependency, modulePath+"/") {
				continue
			}
			if inLayer(current.ImportPath, "pkg") &&
				!publicModuleDependencyAllowed(current.ImportPath, dependency) {
				violations = append(violations,
					current.ImportPath+" has unreviewed public dependency "+dependency,
				)
			}
			if consumers, concrete := allowedConcreteAdapterConsumers[dependency]; concrete && !stringAllowed(consumers, current.ImportPath) {
				violations = append(violations,
					current.ImportPath+" imports concrete adapter "+dependency+
						" outside a reviewed composition root",
				)
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

func publicModuleDependencyAllowed(importPath, dependency string) bool {
	allowed, found := allowedPublicModuleDependencies[importPath]
	if !found {
		return false
	}
	return stringAllowed(allowed, dependency)
}

func stringAllowed(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
