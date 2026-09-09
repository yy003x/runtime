package authruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/infrastructure/codexauth"
)

type scriptedConsole struct {
	commands []string
	choices  []string
	views    []ConsoleView
	choose   func(ConsoleView)
	wait     func(context.Context, <-chan LoginNotice) (string, error)
}

func (ui *scriptedConsole) Command(_ context.Context, view ConsoleView) (string, error) {
	ui.views = append(ui.views, view)
	if len(ui.commands) == 0 {
		return "/exit", nil
	}
	command := ui.commands[0]
	ui.commands = ui.commands[1:]
	return command, nil
}

func (ui *scriptedConsole) Choose(_ context.Context, view ConsoleView, choices []ConsoleChoice) (string, error) {
	ui.views = append(ui.views, view)
	if ui.choose != nil {
		ui.choose(view)
	}
	if len(ui.choices) == 0 {
		return choices[0].Value, nil
	}
	choice := ui.choices[0]
	ui.choices = ui.choices[1:]
	return choice, nil
}

func (ui *scriptedConsole) Wait(ctx context.Context, view ConsoleView, notices <-chan LoginNotice) (string, error) {
	ui.views = append(ui.views, view)
	if ui.wait != nil {
		return ui.wait(ctx, notices)
	}
	for {
		select {
		case _, ok := <-notices:
			if !ok {
				return "", nil
			}
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func consoleService(t *testing.T, transport http.RoundTripper) (*Service, codexauth.Store) {
	t.Helper()
	store := codexauth.Store{StateDir: filepath.Join(t.TempDir(), "state")}
	if transport == nil {
		transport = transportFunc(func(*http.Request) (*http.Response, error) {
			t.Error("unexpected auth request")
			return nil, errors.New("unexpected")
		})
	}
	return New(store, codexauth.NewClient(&http.Client{Transport: transport})), store
}

func runScript(t *testing.T, service *Service, ui *scriptedConsole, initial string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := (&Console{Service: service, UI: ui}).Run(ctx, initial); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleReadOnlyCommandsAndUnknownInput(t *testing.T) {
	service, store := consoleService(t, nil)
	ui := &scriptedConsole{commands: []string{"/status", "/help", "/logout", "/logout --all", "/login fixture-secret", "fixture-token", "/exit"}}
	runScript(t, service, ui, "")
	if _, err := os.Stat(store.StateDir); !os.IsNotExist(err) {
		t.Fatal("read-only console created state")
	}
	for _, view := range ui.views {
		if strings.Contains(fmt.Sprintf("%+v", view), "fixture-") {
			t.Fatal("unknown input echoed")
		}
	}
}

func TestConsoleStatusExplainsAccessTokenExpiryWithoutRefreshing(t *testing.T) {
	service, store := consoleService(t, nil)
	seed(t, store, true)
	before, err := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	ui := &scriptedConsole{commands: []string{"/status", "/exit"}}
	runScript(t, service, ui, "")
	view := ui.views[len(ui.views)-1]
	if !view.Status.LoggedIn || !view.Status.NeedsRefresh || !strings.Contains(view.Message, "访问令牌到期") || !strings.Contains(view.Message, "不代表届时必须重新登录") {
		t.Fatal("status did not distinguish token expiry from login lifetime")
	}
	after, err := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	if err != nil || string(before) != string(after) {
		t.Fatal("status refreshed an expired credential")
	}
}

func TestConsoleLogoutDefaultCancelAndReconfirmation(t *testing.T) {
	for _, confirm := range []bool{false, true} {
		t.Run(fmt.Sprint(confirm), func(t *testing.T) {
			service, store := consoleService(t, nil)
			seed(t, store, false)
			before, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
			ui := &scriptedConsole{}
			runScript(t, service, ui, "/logout")
			after, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
			if string(before) != string(after) {
				t.Fatal("default confirmation changed credentials")
			}
			confirmations := 0
			ui = &scriptedConsole{choices: []string{"logout", "cancel"}, choose: func(view ConsoleView) {
				confirmations++
				if confirmations == 1 {
					if err := store.Update(context.Background(), func(doc *codexauth.Document) (bool, error) {
						credential := doc.Providers[codexauth.Provider]
						credential.AccountID = "fixture-new-account"
						doc.Providers[codexauth.Provider] = credential
						return true, nil
					}); err != nil {
						t.Fatal(err)
					}
				} else if !strings.Contains(view.Message, "重新确认") {
					t.Fatal("missing changed-account warning")
				}
			}}
			if confirm {
				ui.choices[1] = "logout"
			}
			runScript(t, service, ui, "/logout")
			doc, err := store.Read()
			if err != nil {
				t.Fatal(err)
			}
			if confirmations != 2 || (len(doc.Providers) == 0) != confirm {
				t.Fatalf("confirmations=%d removed=%t", confirmations, len(doc.Providers) == 0)
			}
		})
	}
}

func TestConsoleLoginConfirmationGuardsNewAccountBeforeNetwork(t *testing.T) {
	service, store := consoleService(t, nil)
	seed(t, store, false)
	ui := &scriptedConsole{choices: []string{"login", "device"}, choose: func(view ConsoleView) {
		if view.Title == "选择登录方式" {
			seed(t, store, false)
		}
	}}
	runScript(t, service, ui, "/login")
	if !strings.Contains(ui.views[len(ui.views)-1].Message, "未覆盖") {
		t.Fatal("missing stale-confirmation result")
	}
}

func TestConsoleDeviceCancelJoinsAndPreservesCredentials(t *testing.T) {
	service, store := consoleService(t, transportFunc(func(request *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(request.URL.Path, "/usercode") {
			t.Error("cancelled flow kept polling")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"device_auth_id":"fixture-device","user_code":"ABCD-TEST","interval":1}`)), Header: make(http.Header)}, nil
	}))
	seed(t, store, false)
	before, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	ui := &scriptedConsole{choices: []string{"login", "device"}, wait: func(ctx context.Context, notices <-chan LoginNotice) (string, error) {
		select {
		case <-notices:
			return "cancel", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}}
	runScript(t, service, ui, "/login")
	after, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	if string(before) != string(after) {
		t.Fatal("cancelled login changed credentials")
	}
	if !strings.Contains(ui.views[len(ui.views)-1].Message, "已取消登录") {
		t.Fatal("missing cancel result")
	}
}

func TestConsoleFailureDoesNotRetryWithoutUserChoice(t *testing.T) {
	calls := 0
	service, store := consoleService(t, transportFunc(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 503, Body: io.NopCloser(strings.NewReader(`{"error":"fixture-secret"}`)), Header: make(http.Header)}, nil
	}))
	seed(t, store, false)
	before, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	ui := &scriptedConsole{choices: []string{"login", "device", "retry", "back"}}
	runScript(t, service, ui, "/login")
	if calls != 2 {
		t.Fatalf("requests=%d", calls)
	}
	after, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	if string(before) != string(after) {
		t.Fatal("failed login changed credentials")
	}
	for _, view := range ui.views {
		if strings.Contains(fmt.Sprint(view), "fixture-secret") {
			t.Fatal("provider response leaked")
		}
	}
}

func TestConsoleUnreadableStoreNeverDeletesOrAuthorizes(t *testing.T) {
	service, store := consoleService(t, nil)
	if err := os.MkdirAll(store.StateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.StateDir, "auth.json")
	if err := os.WriteFile(path, []byte("fixture-invalid-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	ui := &scriptedConsole{commands: []string{"/status", "/login", "/logout", "/exit"}}
	runScript(t, service, ui, "")
	data, _ := os.ReadFile(path)
	if string(data) != "fixture-invalid-json" {
		t.Fatal("invalid store changed")
	}
	if ui.views[len(ui.views)-1].StatusError == "" {
		t.Fatal("missing read error")
	}
}
