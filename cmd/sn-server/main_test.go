package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/internal/layout"
)

func TestLoadServerConfigUsesLoopbackDefaults(t *testing.T) {
	config, err := loadServerConfig(func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	if config.Address != "127.0.0.1:8080" {
		t.Fatalf("address=%q", config.Address)
	}
	if config.BearerToken != "" {
		t.Fatal("default loopback server should not invent a persistent token")
	}
}

func TestLoadServerConfigRequiresTokenOutsideLoopback(t *testing.T) {
	_, err := loadServerConfig(func(name string) string {
		if name == "HTTP_ADDR" {
			return ":8080"
		}
		return ""
	})
	if err == nil {
		t.Fatal("non-loopback address without token was accepted")
	}

	config, err := loadServerConfig(func(name string) string {
		switch name {
		case "HTTP_ADDR":
			return "0.0.0.0:8080"
		case "SN_SERVER_TOKEN":
			return "secret"
		default:
			return ""
		}
	})
	if err != nil || config.BearerToken != "secret" {
		t.Fatalf("config=%#v err=%v", config, err)
	}
}

func TestServerEntryRejectsActivationGuard(t *testing.T) {
	paths, err := layout.FromHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.StateDir, "activation.guard.json"),
		[]byte("{}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := requireActivationReady(paths); err == nil {
		t.Fatal("sn-server entry accepted an active activation guard")
	}
}

func TestServerEntryRejectsArguments(t *testing.T) {
	if err := validateServerArgs([]string{"--unexpected"}); err == nil {
		t.Fatal("sn-server accepted an unexpected argument")
	}
	if err := validateServerArgs(nil); err != nil {
		t.Fatal(err)
	}
}
