package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/contract"
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

func TestBearerAuthUsesCanonicalErrorEnvelope(t *testing.T) {
	handler := bearerAuth("secret", http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	request.Header.Set("Authorization", "Bearer wrong")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized ||
		response.Header().Get("WWW-Authenticate") != "Bearer" ||
		response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf(
			"status=%d headers=%v body=%s",
			response.Code, response.Header(), response.Body.String(),
		)
	}
	var payload struct {
		Error contract.RuntimeError `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Code != contract.ErrorAuthenticationFailed ||
		payload.Error.Phase != contract.PhaseTransport {
		t.Fatalf("error=%#v", payload.Error)
	}
	if response.Body.String() == "" ||
		payload.Error.Message == "" {
		t.Fatal("canonical authentication error is empty")
	}
}
