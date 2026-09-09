package cli

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/yy003x/runtime/internal/application/authruntime"
	"github.com/yy003x/runtime/internal/infrastructure/codexauth"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/infrastructure/terminalinput"
)

type authPTY struct {
	master  *os.File
	slave   *os.File
	done    chan struct{}
	chunks  chan string
	err     error
	output  string
	pending string
}

func startAuthPTY(t *testing.T, paths layout.Paths, initial string, transport http.RoundTripper) *authPTY {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := pty.Setsize(slave, &pty.Winsize{Cols: 90, Rows: 26}); err != nil {
		t.Fatal(err)
	}
	if transport == nil {
		transport = rawRoundTripper(func(*http.Request) (*http.Response, error) {
			t.Error("unexpected provider request")
			return nil, fmt.Errorf("unexpected")
		})
	}
	console := authruntime.Console{
		Service:     authruntime.New(codexauth.Store{StateDir: paths.StateDir}, codexauth.NewClient(&http.Client{Transport: transport})),
		OpenBrowser: func(context.Context, string) error { return fmt.Errorf("test browser disabled") },
	}
	screen := &authPTY{master: master, slave: slave, done: make(chan struct{}), chunks: make(chan string, 128)}
	readerDone := make(chan struct{})
	go func() {
		defer close(screen.chunks)
		buffer := make([]byte, 4096)
		for {
			n, err := master.Read(buffer)
			if n > 0 {
				select {
				case screen.chunks <- string(buffer[:n]):
				case <-readerDone:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()
	go func() { screen.err = runAuthConsoleUI(paths, slave, slave, initial, console); close(screen.done) }()
	t.Cleanup(func() {
		master.Close()
		select {
		case <-screen.done:
		case <-time.After(3 * time.Second):
			t.Error("console failed to stop after terminal closed")
		}
		close(readerDone)
		slave.Close()
	})
	return screen
}

func (screen *authPTY) send(t *testing.T, text string) {
	t.Helper()
	if _, err := screen.master.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
}

func (screen *authPTY) expect(t *testing.T, text string) {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		if index := strings.Index(screen.pending, text); index >= 0 {
			screen.pending = screen.pending[index+len(text):]
			return
		}
		select {
		case chunk, ok := <-screen.chunks:
			if !ok {
				t.Fatalf("PTY closed before %q", text)
			}
			screen.pending += chunk
			screen.output += chunk
		case <-deadline.C:
			t.Fatalf("PTY did not display %q; last screen: %s", text, screen.pending)
		}
	}
}

func (screen *authPTY) exit(t *testing.T) {
	t.Helper()
	screen.send(t, "\x15/exit\r")
	screen.expect(t, "\x1b[?1049l")
	select {
	case <-screen.done:
		if screen.err != nil {
			t.Fatal(screen.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("console did not exit")
	}
}

func seedAuthPTY(t *testing.T, paths layout.Paths) {
	t.Helper()
	err := (codexauth.Store{StateDir: paths.StateDir}).Update(context.Background(), func(doc *codexauth.Document) (bool, error) {
		doc.Providers[codexauth.Provider] = codexauth.Credential{Type: "oauth", AccessToken: "fixture-access", RefreshToken: "fixture-refresh", AccountID: "fixture-account", ExpiresAt: time.Now().Add(time.Hour)}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuthTUICommandsCompletionAndReadOnlyHome(t *testing.T) {
	paths, _ := layout.FromHome(t.TempDir())
	screen := startAuthPTY(t, paths, "", nil)
	screen.expect(t, "未登录")
	screen.send(t, "/")
	screen.expect(t, "/logout")
	screen.send(t, "\t")
	screen.expect(t, "› /login▏")
	// Tab only completes; clearing it must not start a login flow.
	screen.send(t, "\x15/logout\r")
	screen.expect(t, "当前没有已登录服务")
	screen.send(t, "/help\r")
	screen.expect(t, "凭证属于当前 Runtime home")
	screen.send(t, "\x1b")
	screen.expect(t, "输入 / 查看命令")
	screen.send(t, "/status\r")
	screen.expect(t, "已重新读取本地状态")
	screen.exit(t)
	entries, _ := os.ReadDir(paths.Home)
	if len(entries) != 0 {
		t.Fatal("read-only TUI created runtime files")
	}
}

func TestAuthTUILogoutConfirmationAndExitPreserveLogin(t *testing.T) {
	paths, _ := layout.FromHome(t.TempDir())
	seedAuthPTY(t, paths)
	before, _ := os.ReadFile(paths.AuthFile)
	screen := startAuthPTY(t, paths, "/logout", nil)
	screen.expect(t, "退出 ChatGPT / Codex 登录？")
	screen.expect(t, "› 取消")
	screen.send(t, "\r")
	screen.expect(t, "已取消退出登录")
	after, _ := os.ReadFile(paths.AuthFile)
	if string(before) != string(after) {
		t.Fatal("default selection cleared credentials")
	}
	screen.exit(t)
	after, _ = os.ReadFile(paths.AuthFile)
	if string(before) != string(after) {
		t.Fatal("exit cleared credentials")
	}
	second := startAuthPTY(t, paths, "/logout", nil)
	second.expect(t, "› 取消")
	second.send(t, "\x1b[B\r")
	second.expect(t, "本地凭证已清除")
	second.exit(t)
	status, err := (codexauth.Store{StateDir: paths.StateDir}).Read()
	if err != nil || len(status.Providers) != 0 {
		t.Fatal("logout did not clear credential")
	}
	for _, secret := range []string{"fixture-access", "fixture-refresh", "fixture-account"} {
		if strings.Contains(screen.output+second.output, secret) {
			t.Fatal("credential leaked to TUI")
		}
	}
}

func TestAuthTUIAutomaticRefreshStatusIsReadOnly(t *testing.T) {
	for _, expiresSoon := range []bool{false, true} {
		t.Run(fmt.Sprint(expiresSoon), func(t *testing.T) {
			paths, _ := layout.FromHome(t.TempDir())
			seedAuthPTY(t, paths)
			if expiresSoon {
				if err := (codexauth.Store{StateDir: paths.StateDir}).Update(context.Background(), func(document *codexauth.Document) (bool, error) {
					credential := document.Providers[codexauth.Provider]
					credential.ExpiresAt = time.Now().Add(4 * time.Minute)
					document.Providers[codexauth.Provider] = credential
					return true, nil
				}); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(paths.AuthFile)
			if err != nil {
				t.Fatal(err)
			}
			screen := startAuthPTY(t, paths, "", nil)
			if expiresSoon {
				screen.expect(t, "下次调用自动续期")
			} else {
				screen.expect(t, "支持自动续期")
			}
			screen.expect(t, "Ctrl+C 退出")
			if strings.Contains(screen.output, "到期") {
				t.Fatal("home exposed token expiry as a login deadline")
			}
			screen.send(t, "/status\r")
			screen.expect(t, "访问令牌到期")
			screen.exit(t)
			after, err := os.ReadFile(paths.AuthFile)
			if err != nil || string(before) != string(after) {
				t.Fatal("opening the TUI or status refreshed credentials")
			}
		})
	}
}

func TestAuthTUIDeviceSuccessAndCancellation(t *testing.T) {
	for _, success := range []bool{true, false} {
		t.Run(fmt.Sprint(success), func(t *testing.T) {
			paths, _ := layout.FromHome(t.TempDir())
			if !success {
				seedAuthPTY(t, paths)
			}
			before, _ := os.ReadFile(paths.AuthFile)
			access := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-new-account"}}`)) + ".fixture"
			transport := rawRoundTripper(func(request *http.Request) (*http.Response, error) {
				code, body := 200, ""
				switch request.URL.Path {
				case "/api/accounts/deviceauth/usercode":
					body = `{"device_auth_id":"fixture-device","user_code":"ABCD-TEST","interval":1}`
				case "/api/accounts/deviceauth/token":
					if success {
						body = `{"authorization_code":"fixture-code","code_verifier":"fixture-verifier"}`
					} else {
						code = 404
						body = `{}`
					}
				case "/oauth/token":
					body = fmt.Sprintf(`{"access_token":%q,"refresh_token":"fixture-rotated-refresh","expires_in":3600}`, access)
				default:
					t.Error("unexpected endpoint")
					code = 500
					body = `{}`
				}
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			screen := startAuthPTY(t, paths, "/login", transport)
			if !success {
				screen.expect(t, "当前已有登录")
				screen.send(t, "\x1b[B\r")
			}
			screen.expect(t, "选择登录方式")
			screen.send(t, "\x1b[B\r")
			screen.expect(t, "ABCD-TEST")
			if success {
				screen.expect(t, "登录成功，凭证已保存")
			} else {
				screen.send(t, "\x03")
				screen.expect(t, "已取消登录")
				after, _ := os.ReadFile(paths.AuthFile)
				if string(before) != string(after) {
					t.Fatal("cancellation changed credentials")
				}
			}
			screen.exit(t)
			if strings.Contains(screen.output, access) || strings.Contains(screen.output, "fixture-rotated-refresh") || strings.Contains(screen.output, "fixture-new-account") {
				t.Fatal("secret rendered")
			}
			for _, path := range []string{paths.RunDBFile, paths.LogsDir, paths.SessionsDir} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("auth created %s", path)
				}
			}
		})
	}
}

func TestAuthTUIWrappingAndSmallScreenFocus(t *testing.T) {
	lines := wrapAuthLines([]string{"你好ab中", "x\x1b[2J\x00\u202ey"}, 5)
	if lines[0] != "你好a" || lines[1] != "b中" {
		t.Fatalf("bad CJK wrapping: %q", lines)
	}
	if strings.Contains(strings.Join(lines, ""), "\x1b") || strings.Contains(strings.Join(lines, ""), "\u202e") {
		t.Fatal("terminal controls survived")
	}
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	defer slave.Close()
	terminal, err := terminalinput.Open(slave)
	if err != nil {
		t.Fatal(err)
	}
	defer terminal.Close()
	pty.Setsize(slave, &pty.Winsize{Rows: 12, Cols: 40})
	var output strings.Builder
	ui := &authTerminalUI{terminal: terminal, output: &output, home: "/fixture/home", follow: true}
	view := authruntime.ConsoleView{Title: "确认", Message: strings.Repeat("说明", 80)}
	if err := ui.render(view, []string{"  取消", "› 退出登录"}, "Enter 确认"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "› 退出登录") || len(strings.Split(output.String(), "\r\n")) > 12 {
		t.Fatal("focused action outside small screen")
	}
	before := output.Len()
	ui.render(view, []string{"  取消", "› 退出登录"}, "Enter 确认")
	if output.Len() != before {
		t.Fatal("unchanged frame redrawn")
	}
}
