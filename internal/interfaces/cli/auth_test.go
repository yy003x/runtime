package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/infrastructure/codexauth"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/pkg/contract"
)

func TestAuthStatusMainIsReadOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	output := captureStdout(t, func() {
		if code := Main([]string{"--json", "auth", "status", "openai-codex"}); code != 0 {
			t.Fatalf("exit=%d", code)
		}
	})
	var response struct {
		OK   bool `json:"ok"`
		Auth struct {
			LoggedIn bool `json:"logged_in"`
		} `json:"auth"`
		Contract int `json:"contract_version"`
	}
	if err := json.Unmarshal([]byte(output), &response); err != nil || !response.OK || response.Auth.LoggedIn || response.Contract != contract.RuntimeContractVersion {
		t.Fatalf("output=%s err=%v", output, err)
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatalf("status created files: %v", entries)
	}
}

func TestAuthStatusWithoutProviderAndNonTTYConsole(t *testing.T) {
	paths, _ := layout.FromHome(t.TempDir())
	for _, jsonOutput := range []bool{false, true} {
		for _, args := range [][]string{nil, {"login"}, {"logout"}} {
			var out, diagnostic strings.Builder
			err := runAuthNamespace(paths, args, newCLIOutput(jsonOutput, &out, &diagnostic))
			if err == nil || !strings.Contains(err.Error(), "interactive terminal") || out.Len() != 0 || diagnostic.Len() != 0 {
				t.Fatalf("noninteractive auth did not fail before interaction: %v", err)
			}
		}
		var out strings.Builder
		if err := runAuthNamespace(paths, []string{"status"}, newCLIOutput(jsonOutput, &out, io.Discard)); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "\x1b") {
			t.Fatal("status emitted ANSI")
		}
		if jsonOutput {
			var response map[string]any
			if err := json.Unmarshal([]byte(out.String()), &response); err != nil || response["ok"] != true {
				t.Fatal("invalid status JSON")
			}
		}
	}
	entries, _ := os.ReadDir(paths.Home)
	if len(entries) != 0 {
		t.Fatal("noninteractive entry created files")
	}
}

func TestAuthStrictGrammar(t *testing.T) {
	paths, _ := layout.FromHome(t.TempDir())
	for _, args := range [][]string{nil, {"login"}, {"login", "unknown"}, {"refresh", "openai-codex"}, {"status", "openai-codex", "--no-browser"}, {"login", "openai-codex", "--device-code", "--no-browser"}, {"login", "openai-codex", "--device-code", "--device-code"}, {"login", "openai-codex", "--token", "fixture-secret"}} {
		err := runAuthNamespace(paths, args, newCLIOutput(true, io.Discard, io.Discard))
		if err == nil || strings.Contains(err.Error(), "fixture-secret") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
	if _, err := os.Stat(paths.StateDir); !os.IsNotExist(err) {
		t.Fatal("invalid commands created state")
	}
}

func TestAuthStatusReportsAutomaticRefreshWithoutNetwork(t *testing.T) {
	paths, _ := layout.FromHome(t.TempDir())
	seedAuthPTY(t, paths)
	if err := (codexauth.Store{StateDir: paths.StateDir}).Update(context.Background(), func(document *codexauth.Document) (bool, error) {
		credential := document.Providers[codexauth.Provider]
		credential.ExpiresAt = time.Now().Add(4 * time.Minute)
		document.Providers[codexauth.Provider] = credential
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	original := http.DefaultTransport
	http.DefaultTransport = rawRoundTripper(func(*http.Request) (*http.Response, error) {
		t.Error("status attempted to refresh credentials")
		return nil, fmt.Errorf("unexpected network request")
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	before, err := os.ReadFile(paths.AuthFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, jsonOutput := range []bool{false, true} {
		var output strings.Builder
		if err := runAuthNamespace(paths, []string{"status"}, newCLIOutput(jsonOutput, &output, io.Discard)); err != nil {
			t.Fatal(err)
		}
		if jsonOutput {
			var response struct {
				OK   bool `json:"ok"`
				Auth struct {
					LoggedIn     bool      `json:"logged_in"`
					NeedsRefresh bool      `json:"needs_refresh"`
					ExpiresAt    time.Time `json:"expires_at"`
				} `json:"auth"`
			}
			if err := json.Unmarshal([]byte(output.String()), &response); err != nil || !response.OK || !response.Auth.LoggedIn || !response.Auth.NeedsRefresh || response.Auth.ExpiresAt.IsZero() {
				t.Fatalf("invalid cached status: %v", err)
			}
		} else if !strings.Contains(output.String(), "Access token expires:") || !strings.Contains(output.String(), "not a login deadline") {
			t.Fatal("human status does not explain automatic refresh")
		}
		for _, secret := range []string{"fixture-access", "fixture-refresh", "fixture-account"} {
			if strings.Contains(output.String(), secret) {
				t.Fatal("status exposed a credential")
			}
		}
	}
	after, err := os.ReadFile(paths.AuthFile)
	if err != nil || string(before) != string(after) {
		t.Fatal("status wrote credentials")
	}
}

func TestCodexCallUsesStoredCredentialWithoutSessionOrRun(t *testing.T) {
	paths := prepareRuntimeHome(t)
	if err := os.WriteFile(filepath.Join(paths.ConfigDir, "api-codex.json"), []byte(`{"type":"api","driver":"openai-codex-responses","endpoint":"https://chatgpt.com/backend-api/codex/responses","model":"fixture","timeout":"1m"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	store := codexauth.Store{StateDir: paths.StateDir}
	if err := store.Update(context.Background(), func(document *codexauth.Document) (bool, error) {
		document.Providers[codexauth.Provider] = codexauth.Credential{Type: "oauth", AccessToken: "fixture-access", RefreshToken: "fixture-refresh", AccountID: "fixture-account", ExpiresAt: time.Now().Add(time.Hour)}
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	original := http.DefaultTransport
	http.DefaultTransport = rawRoundTripper(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || request.Header.Get("Authorization") != "Bearer fixture-access" {
			t.Fatal("wrong Codex auth request")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-fixture\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}]}}\n\n"))}, nil
	})
	t.Cleanup(func() { http.DefaultTransport = original })
	var output strings.Builder
	if err := runTestReq(paths, []string{"api-codex", "hello"}, newCLIOutput(false, &output, io.Discard)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || strings.TrimSpace(output.String()) != "Hello" {
		t.Fatalf("calls=%d output=%s", calls, output.String())
	}
	if _, err := os.Stat(paths.RunDBFile); !os.IsNotExist(err) {
		t.Fatal("call created Run database")
	}
	entries, _ := os.ReadDir(paths.SessionsDir)
	if len(entries) != 0 {
		t.Fatal("call created Session")
	}
	logs, _ := filepath.Glob(filepath.Join(paths.LogsDir, "*", "api.jsonl"))
	if len(logs) != 1 {
		t.Fatalf("logs=%v", logs)
	}
	data, _ := os.ReadFile(logs[0])
	for _, secret := range []string{"fixture-access", "fixture-refresh", "fixture-account"} {
		if strings.Contains(string(data), secret) {
			t.Fatal("secret leaked into log")
		}
	}
	if err := runAuthNamespace(paths, []string{"logout", "openai-codex"}, newCLIOutput(true, io.Discard, io.Discard)); err != nil {
		t.Fatal(err)
	}
	if err := runTestReq(paths, []string{"api-codex", "hello"}, newCLIOutput(false, io.Discard, io.Discard)); err == nil {
		t.Fatal("call succeeded after logout")
	}
	if calls != 1 {
		t.Fatal("logged-out call contacted provider")
	}
}

func TestCodexCallMissingContentTypeOutputAndLogs(t *testing.T) {
	const delta = `{"type":"response.output_text.delta","delta":"Hel"}`
	const terminal = `{"type":"response.completed","response":{"id":"resp-fixture","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]}]}}`
	frames := []json.RawMessage{json.RawMessage(delta), json.RawMessage(terminal)}
	completedItemFrames := []json.RawMessage{
		json.RawMessage(delta),
		json.RawMessage(`{"type":"response.output_item.done","output_index":0,"item":{"id":"msg-fixture","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]}}`),
		json.RawMessage(`{"type":"response.completed","response":{"id":"resp-fixture","status":"completed","output":[]}}`),
	}
	for _, test := range []struct {
		name, mode, contentType string
		status                  int
		frames                  []json.RawMessage
		wantError               bool
	}{
		{"text", "text", "", 200, frames, false},
		{"json", "json", "", 200, frames, false},
		{"stream", "stream", "", 200, frames, false},
		{"raw", "raw", "", 200, frames, false},
		{"raw_single_frame", "raw", "", 200, frames[1:], false},
		{"raw_mislabeled", "raw", "application/json", 200, frames, false},
		{"raw_truncated", "raw", "", 200, frames[:1], true},
		{"raw_http_error", "raw", "", 403, []json.RawMessage{json.RawMessage(`{"error":{"message":"fixture denied"}}`)}, true},
		{"completed_items_text", "text", "", 200, completedItemFrames, false},
		{"completed_items_json", "json", "", 200, completedItemFrames, false},
		{"completed_items_stream", "stream", "", 200, completedItemFrames, false},
		{"completed_items_raw", "raw", "", 200, completedItemFrames, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			mode := test.mode
			paths := prepareRuntimeHome(t)
			if err := os.WriteFile(filepath.Join(paths.ConfigDir, "api-codex.json"), []byte(`{"type":"api","driver":"openai-codex-responses","endpoint":"https://chatgpt.com/backend-api/codex/responses","model":"fixture","timeout":"1m"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			seedAuthPTY(t, paths)
			calls := 0
			var requestBody []byte
			original := http.DefaultTransport
			http.DefaultTransport = rawRoundTripper(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.String() != "https://chatgpt.com/backend-api/codex/responses" || request.Header.Get("Authorization") != "Bearer fixture-access" || request.Header.Get("Accept") != "text/event-stream" {
					t.Fatal("wrong Codex request")
				}
				var err error
				requestBody, err = io.ReadAll(request.Body)
				if err != nil {
					t.Fatal(err)
				}
				var body strings.Builder
				for _, data := range test.frames {
					var frame struct {
						Type string `json:"type"`
					}
					if err := json.Unmarshal(data, &frame); err != nil {
						t.Fatal(err)
					}
					if test.status < 300 {
						fmt.Fprintf(&body, "event: %s\ndata: %s\n\n", frame.Type, data)
					} else {
						body.Write(data)
					}
				}
				headers := make(http.Header)
				if test.contentType != "" {
					headers.Set("Content-Type", test.contentType)
				}
				return &http.Response{StatusCode: test.status, Header: headers, Body: io.NopCloser(strings.NewReader(body.String()))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			args := []string{"api-codex"}
			if mode == "stream" || mode == "raw" {
				args = append(args, "--"+mode)
			}
			args = append(args, "hello")
			var stdout, stderr strings.Builder
			if err := runTestReq(paths, args, newCLIOutput(mode == "json", &stdout, &stderr)); (err != nil) != test.wantError {
				t.Fatalf("err=%v wantError=%t", err, test.wantError)
			}
			if calls != 1 || stderr.Len() != 0 {
				t.Fatalf("calls=%d stderr=%s", calls, stderr.String())
			}
			switch mode {
			case "text":
				if stdout.String() != "Hello\n" {
					t.Fatalf("output=%s", stdout.String())
				}
			case "raw":
				request, response := decodeRawCall(t, []byte(stdout.String()))
				assertRawJSONEqual(t, request, requestBody)
				want, err := json.Marshal(test.frames)
				if err != nil {
					t.Fatal(err)
				}
				if test.status >= 300 {
					want = test.frames[0]
				}
				assertRawJSONEqual(t, response, want)
			default:
				for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
					if !json.Valid([]byte(line)) {
						t.Fatalf("invalid %s JSON: %s", mode, line)
					}
				}
				if !strings.Contains(stdout.String(), `"state":"completed"`) || !strings.Contains(stdout.String(), "Hello") {
					t.Fatalf("incomplete %s output: %s", mode, stdout.String())
				}
			}
			logs, err := filepath.Glob(filepath.Join(paths.LogsDir, "*", "api.jsonl"))
			if err != nil || len(logs) != 1 {
				t.Fatalf("logs=%v err=%v", logs, err)
			}
			data, err := os.ReadFile(logs[0])
			if err != nil {
				t.Fatal(err)
			}
			var logged rawCallLog
			if err := json.Unmarshal(data, &logged); err != nil || (logged.Error != nil) != test.wantError || len(logged.Attempts) != 1 || logged.Attempts[0].Response == nil {
				t.Fatalf("log=%s err=%v", data, err)
			}
			response := logged.Attempts[0].Response
			if response.Status != test.status || len(response.Data) != len(test.frames) {
				t.Fatalf("missing response frames: %+v", response)
			}
			if _, present := response.Headers["Content-Type"]; present != (test.contentType != "") || response.Headers["Content-Type"] != test.contentType {
				t.Fatal("log changed the response Content-Type")
			}
			for index, frame := range test.frames {
				assertRawJSONEqual(t, response.Data[index], frame)
			}
			for _, secret := range []string{"fixture-access", "fixture-refresh", "fixture-account"} {
				if strings.Contains(string(data), secret) || strings.Contains(stdout.String(), secret) {
					t.Fatal("credential leaked in log or output")
				}
			}
			if _, err := os.Stat(paths.RunDBFile); !os.IsNotExist(err) {
				t.Fatal("call created Run database")
			}
			if entries, err := os.ReadDir(paths.SessionsDir); err != nil && !os.IsNotExist(err) || len(entries) != 0 {
				t.Fatal("call created Session")
			}
		})
	}
}

func TestCodexCallAutomaticallyRefreshesCredentials(t *testing.T) {
	for _, test := range []struct {
		name          string
		validFor      time.Duration
		unauthorized  bool
		alwaysReject  bool
		refreshStatus int
		wantAPI       int
		wantError     bool
	}{
		{"expiring", 4 * time.Minute, false, false, 200, 1, false},
		{"expired", -time.Minute, false, false, 200, 1, false},
		{"401_refresh", time.Hour, true, false, 200, 2, false},
		{"401_only_once", time.Hour, true, true, 200, 2, true},
		{"refresh_revoked", -time.Minute, false, false, 401, 0, true},
		{"refresh_unavailable", -time.Minute, false, false, 503, 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			paths := prepareRuntimeHome(t)
			if err := os.WriteFile(filepath.Join(paths.ConfigDir, "api-codex.json"), []byte(`{"type":"api","driver":"openai-codex-responses","endpoint":"https://chatgpt.com/backend-api/codex/responses","model":"fixture","timeout":"1m"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			store := codexauth.Store{StateDir: paths.StateDir}
			if err := store.Update(context.Background(), func(document *codexauth.Document) (bool, error) {
				document.Providers[codexauth.Provider] = codexauth.Credential{
					Type: "oauth", AccessToken: "fixture-old-access", RefreshToken: "fixture-refresh",
					AccountID: "fixture-account", ExpiresAt: time.Now().Add(test.validFor),
				}
				return true, nil
			}); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(paths.AuthFile)
			if err != nil {
				t.Fatal(err)
			}
			access := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`)) + ".rotated"
			refreshes, requests := 0, 0
			original := http.DefaultTransport
			http.DefaultTransport = rawRoundTripper(func(request *http.Request) (*http.Response, error) {
				if request.Method != http.MethodPost {
					t.Fatal("automatic refresh attempted a login/browser request")
				}
				code, contentType, body := 200, "application/json", ""
				switch request.URL.String() {
				case "https://auth.openai.com/oauth/token":
					refreshes++
					if err := request.ParseForm(); err != nil {
						t.Fatal(err)
					}
					if request.Form.Get("grant_type") != "refresh_token" || request.Form.Get("refresh_token") != "fixture-refresh" || request.Form.Get("client_id") != codexauth.ClientID {
						t.Fatal("wrong refresh grant or rotated token refreshed twice")
					}
					code = test.refreshStatus
					body = fmt.Sprintf(`{"access_token":%q,"refresh_token":"fixture-rotated-refresh","expires_in":3600}`, access)
					if code != 200 {
						body = `{"error":"fixture-auth-private-error"}`
					}
				case "https://chatgpt.com/backend-api/codex/responses":
					requests++
					wantToken := access
					if test.unauthorized && requests == 1 {
						wantToken = "fixture-old-access"
					}
					if request.Header.Get("Authorization") != "Bearer "+wantToken || request.Header.Get("Chatgpt-Account-Id") != "fixture-account" {
						t.Fatal("API call did not use the expected credential")
					}
					if test.unauthorized && (requests == 1 || test.alwaysReject) {
						code, body = 401, `{"error":{"message":"Unauthorized"}}`
					} else {
						contentType = "text/event-stream"
						body = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-fixture\",\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]}]}}\n\n"
					}
				default:
					t.Fatal("automatic refresh contacted an unexpected endpoint")
				}
				return &http.Response{StatusCode: code, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			var output strings.Builder
			err = runTestReq(paths, []string{"api-codex", "hello"}, newCLIOutput(false, &output, io.Discard))
			if (err != nil) != test.wantError || refreshes != 1 || requests != test.wantAPI {
				t.Fatalf("refreshes=%d API attempts=%d err=%v", refreshes, requests, err)
			}
			visible := output.String() + fmt.Sprint(err)
			if !test.wantError {
				if strings.TrimSpace(output.String()) != "Hello" {
					t.Fatalf("unexpected result: %q", output.String())
				}
				// Recompose the public CLI path; it must load the new token from disk.
				if err := runTestReq(paths, []string{"api-codex", "again"}, newCLIOutput(false, io.Discard, io.Discard)); err != nil {
					t.Fatal(err)
				}
				if refreshes != 1 || requests != test.wantAPI+1 {
					t.Fatal("subsequent call did not reuse the rotated credential")
				}
			}
			document, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if test.refreshStatus == 200 {
				credential := document.Providers[codexauth.Provider]
				if document.Revision != 2 || credential.AccessToken != access || credential.RefreshToken != "fixture-rotated-refresh" || !credential.ExpiresAt.After(time.Now().Add(50*time.Minute)) {
					t.Fatal("rotated credential was not persisted correctly")
				}
			} else {
				after, err := os.ReadFile(paths.AuthFile)
				if err != nil || string(before) != string(after) {
					t.Fatal("refresh failure changed the credential cache")
				}
			}
			if _, err := os.Stat(paths.RunDBFile); !os.IsNotExist(err) {
				t.Fatal("refresh/call created a Run database")
			}
			if _, err := os.Stat(paths.SessionsDir); !os.IsNotExist(err) {
				t.Fatal("refresh/call created a Session directory")
			}
			logs, err := filepath.Glob(filepath.Join(paths.LogsDir, "*", "api.jsonl"))
			if err != nil || (len(logs) == 1) != (requests > 0) {
				t.Fatalf("unexpected call logs: count=%d err=%v", len(logs), err)
			}
			for _, path := range logs {
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				visible += string(data)
			}
			for _, secret := range []string{"fixture-old-access", access, "fixture-refresh", "fixture-rotated-refresh", "fixture-account", "fixture-auth-private-error"} {
				if strings.Contains(visible, secret) {
					t.Fatal("automatic refresh exposed a credential or auth response")
				}
			}
		})
	}
}
