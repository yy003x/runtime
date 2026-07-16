package main

import "testing"

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
