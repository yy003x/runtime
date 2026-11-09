//go:build darwin || linux

package codingtui

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/creack/pty"

	"github.com/yy003x/sn-code/internal/application/codingagent"
	"github.com/yy003x/sn-code/internal/infrastructure/terminalinput"
)

func testTUIApp(t *testing.T) (*App, *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { master.Close(); slave.Close() })
	terminal, err := terminalinput.Open(slave)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close() })
	return &App{
		term: terminal, out: &bytes.Buffer{}, editor: NewEditor(), cwd: t.TempDir(),
	}, slave
}

func TestClearScreenAtSmallHeights(t *testing.T) {
	app, slave := testTUIApp(t)
	app.history = []string{"first", "second", "third"}
	for _, test := range []struct {
		height uint16
		want   string
	}{
		{1, ""}, {5, ""}, {6, ""},
		{8, "second\nthird\n"}, {24, "first\nsecond\nthird\n"},
	} {
		if err := pty.Setsize(slave, &pty.Winsize{Cols: 80, Rows: test.height}); err != nil {
			t.Fatal(err)
		}
		output := app.out.(*bytes.Buffer)
		output.Reset()
		app.clearScreen()
		want := "\r" + eraseBelow + clearScreen + test.want
		if output.String() != want {
			t.Fatalf("height=%d output=%q, want=%q", test.height, output.String(), want)
		}
	}
}

func TestShellAndRunCompletionOrder(t *testing.T) {
	for _, order := range []string{"rss", "srs", "ssr"} {
		t.Run(order, func(t *testing.T) {
			app, _ := testTUIApp(t)
			app.shells = 2
			app.handleEvent(codingagent.Event{Type: codingagent.EventRunStarted})
			running, shells := true, 2
			for _, completion := range order {
				if completion == 'r' {
					app.handleEvent(codingagent.Event{Type: codingagent.EventRunFinished, Text: "completed"})
					running = false
				} else {
					app.finishUserCommand("done", false)
					shells--
				}
				if app.busy != running || app.shells != shells || app.isBusy() != (running || shells > 0) {
					t.Fatalf("after %c: busy=%v shells=%d", completion, app.busy, app.shells)
				}
			}
		})
	}
}

func TestShellActivityDoesNotImplyActiveRun(t *testing.T) {
	app, _ := testTUIApp(t)
	app.shells = 2
	app.finishUserCommand("first", false)
	if app.busy || !app.isBusy() || app.shells != 1 {
		t.Fatalf("shell-only state: busy=%v shells=%d", app.busy, app.shells)
	}
	app.finishUserCommand("failed", true)
	if app.isBusy() || app.shells != 0 {
		t.Fatalf("completed shells left app busy: %d", app.shells)
	}
}

func TestEditorEditingAndHistory(t *testing.T) {
	editor := NewEditor()
	editor.Insert("hello")
	editor.Left()
	editor.Left()
	editor.Insert("HE") // 光标在第二个 l 前 → hel|lo
	if editor.Value() != "helHElo" || editor.Cursor() != 5 {
		t.Fatalf("insert at cursor: %q cursor=%d", editor.Value(), editor.Cursor())
	}
	editor.Backspace()
	if editor.Value() != "helHlo" {
		t.Fatalf("backspace: %q", editor.Value())
	}
	editor.Delete()
	if editor.Value() != "helHo" {
		t.Fatalf("delete: %q", editor.Value())
	}
	editor.End()
	editor.Insert(" world")
	if editor.Value() != "helHo world" {
		t.Fatalf("end insert: %q", editor.Value())
	}

	if submitted := editor.Submit(); submitted != "helHo world" {
		t.Fatalf("submit: %q", submitted)
	}
	if editor.Value() != "" || editor.Cursor() != 0 {
		t.Fatalf("submit must clear: %q", editor.Value())
	}

	editor.Insert("draft")
	if !editor.HistoryUp() || editor.Value() != "helHo world" {
		t.Fatalf("history up: %q", editor.Value())
	}
	if !editor.HistoryDown() || editor.Value() != "draft" {
		t.Fatalf("history down restores draft: %q", editor.Value())
	}
	editor.Submit()

	// 清空后回到最早历史；无历史时移动返回 false。
	fresh := NewEditor()
	if fresh.HistoryUp() || fresh.HistoryDown() {
		t.Fatal("empty history navigation must report false")
	}
}

func TestEditorSubmitKeepsWhitespaceOnlyOut(t *testing.T) {
	editor := NewEditor()
	editor.Insert("   ")
	if submitted := editor.Submit(); submitted != "   " {
		t.Fatalf("submit returns raw text: %q", submitted)
	}
	// 纯空白不进历史。
	if editor.HistoryUp() {
		t.Fatal("whitespace-only submit must not enter history")
	}
}

func TestStringWidthAndWrap(t *testing.T) {
	if got := stringWidth("abc"); got != 3 {
		t.Fatalf("ascii width = %d", got)
	}
	if got := stringWidth("中文"); got != 4 {
		t.Fatalf("cjk width = %d", got)
	}
	if got := stringWidth("\x1b[31m红\x1b[0m"); got != 2 {
		t.Fatalf("ansi width = %d", got)
	}

	lines := wrapLines("abcdefghij", 4)
	if strings.Join(lines, "|") != "abcd|efgh|ij" {
		t.Fatalf("wrap = %v", lines)
	}
	lines = wrapLines("中文字符", 4)
	if strings.Join(lines, "|") != "中文|字符" {
		t.Fatalf("cjk wrap = %v", lines)
	}
	lines = wrapLines("a\n\nb", 10)
	if strings.Join(lines, "|") != "a||b" {
		t.Fatalf("blank line wrap = %v", lines)
	}
}

func TestTruncateToWidth(t *testing.T) {
	if got := truncateToWidth("hello", 10, "…"); got != "hello" {
		t.Fatalf("short text changed: %q", got)
	}
	if got := truncateToWidth("hello world", 8, "…"); got != "hello w…" {
		t.Fatalf("truncate = %q", got)
	}
	if got := truncateToWidth("中文字符", 5, "…"); got != "中文…" {
		t.Fatalf("cjk truncate = %q", got)
	}
}

func TestBuildFrameSections(t *testing.T) {
	editor := NewEditor()
	editor.Insert("你好")
	view := viewState{
		width:      40,
		status:     "status line",
		editor:     editor,
		footer:     "footer",
		streamTail: []string{"stream1", "stream2"},
		streamMore: true,
	}
	built := buildFrame(view)
	joined := strings.Join(built.lines, "\n")
	for _, want := range []string{"status line", "stream1", "stream2", "…", "你好", "footer"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("frame missing %q:\n%s", want, joined)
		}
	}
	if built.cursorLine != 4 { // 状态1+流式2+折叠1 之后
		t.Fatalf("cursor line = %d", built.cursorLine)
	}
	if built.cursorCol != stringWidth("> ")+4 {
		t.Fatalf("cursor col = %d", built.cursorCol)
	}
}

func TestBuildFrameConfirmAndPicker(t *testing.T) {
	call := codingagent.ToolCallInfoForGate{
		Name:      "bash",
		Effect:    "write_external",
		Risk:      "high",
		Arguments: json.RawMessage(`{"command":"rm -rf /tmp/x"}`),
	}
	view := viewState{width: 60, confirm: &call, footer: "f"}
	built := buildFrame(view)
	joined := strings.Join(built.lines, "\n")
	if !strings.Contains(joined, "bash wants to run") ||
		!strings.Contains(joined, "$ rm -rf /tmp/x") ||
		!strings.Contains(joined, "[y] allow once") {
		t.Fatalf("confirm frame:\n%s", joined)
	}
	if built.cursorLine >= 0 {
		t.Fatal("confirm frame hides cursor")
	}

	picker := &pickerState{
		title:   "switch profile",
		options: []pickerOption{{ID: "a", Label: "a", Detail: "detail"}, {ID: "b", Label: "b"}},
	}
	view = viewState{width: 60, picker: picker, footer: "f"}
	built = buildFrame(view)
	if len(built.lines) != 5 { // 标题 + 2 选项 + 提示 + footer
		t.Fatalf("picker frame lines = %d", len(built.lines))
	}
	picker.move(1)
	picker.move(1) // 环绕回第一项
	if picker.selected != 0 {
		t.Fatalf("picker wrap = %d", picker.selected)
	}
}

func TestSummarizeArguments(t *testing.T) {
	cases := []struct {
		name      string
		arguments string
		want      string
	}{
		{"bash", `{"command":"go test ./..."}`, "$ go test ./..."},
		{"read", `{"path":"main.go"}`, "main.go"},
		{"edit", `{"path":"a.go","edits":[]}`, "a.go"},
		{"unknown", `{"x":1}`, `unknown x=1`},
	}
	for _, item := range cases {
		if got := summarizeArguments(item.name, json.RawMessage(item.arguments)); got != item.want {
			t.Fatalf("summarize(%s) = %q, want %q", item.name, got, item.want)
		}
	}
	if got := summarizeArguments("bash", json.RawMessage("bad")); got != "bash" {
		t.Fatalf("bad json fallback = %q", got)
	}
}
