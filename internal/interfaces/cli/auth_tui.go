package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/yy003x/runtime/internal/application/authruntime"
	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/infrastructure/terminalinput"
)

func runAuthConsole(paths layout.Paths, input, output *os.File, initial string) error {
	console := authruntime.Console{Service: runtimebootstrap.LoadAuthService(paths), OpenBrowser: openAuthBrowser}
	return runAuthConsoleUI(paths, input, output, initial, console)
}

func runAuthConsoleUI(paths layout.Paths, input, output *os.File, initial string, console authruntime.Console) (err error) {
	terminal, err := terminalinput.Open(input)
	if err != nil {
		return fmt.Errorf("open authentication terminal: %w", err)
	}
	defer func() { err = errors.Join(err, terminal.Close()) }()
	// Alternate screen keeps transient OAuth URLs/codes out of shell scrollback.
	defer func() {
		_, restoreErr := io.WriteString(output, "\x1b[?2004l\x1b[?25h\x1b[?1049l")
		err = errors.Join(err, restoreErr)
	}()
	if _, err := io.WriteString(output, "\x1b[?1049h\x1b[?2004h\x1b[?25l"); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	ui := &authTerminalUI{terminal: terminal, output: output, home: paths.Home, authFile: paths.AuthFile}
	console.UI = ui
	err = console.Run(ctx, initial)
	if errors.Is(err, io.EOF) || errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

type authTerminalUI struct {
	terminal *terminalinput.Terminal
	output   io.Writer
	home     string
	authFile string
	scroll   int
	follow   bool
	last     string
}

func (ui *authTerminalUI) read(ctx context.Context) (terminalinput.Key, error) {
	readCtx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	key, err := ui.terminal.ReadKey(readCtx)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		return terminalinput.Key{}, nil
	}
	if key.Name != "" || key.Text != "" {
		ui.follow = true
	}
	return key, err
}

func (ui *authTerminalUI) paging(key terminalinput.Key) bool {
	switch key.Name {
	case "pageup":
		ui.follow = false
		ui.scroll = max(0, ui.scroll-5)
	case "pagedown":
		ui.follow = false
		ui.scroll += 5
	default:
		return false
	}
	return true
}

func authMatches(input string) []authruntime.ConsoleChoice {
	if !strings.HasPrefix(input, "/") {
		return nil
	}
	var matches []authruntime.ConsoleChoice
	for _, spec := range authruntime.ConsoleCommands() {
		value := spec.Name
		if strings.Contains(input, " ") {
			if !spec.Provider {
				continue
			}
			value += " openai-codex"
		}
		if strings.HasPrefix(value, input) {
			matches = append(matches, authruntime.ConsoleChoice{Value: value, Label: value + "  " + spec.Description})
		}
	}
	return matches
}

func (ui *authTerminalUI) Command(ctx context.Context, view authruntime.ConsoleView) (string, error) {
	ui.scroll = 0
	ui.follow = true
	input, cursor, selected, hidden := []rune{}, 0, 0, false
	for {
		matches := authMatches(string(input))
		selected = min(selected, max(0, len(matches)-1))
		body := []string{"输入 / 查看命令，输入 /login 开始登录。", "", "› " + string(input[:cursor]) + "▏" + string(input[cursor:])}
		if !hidden {
			for i, choice := range matches {
				prefix := "  "
				if i == selected {
					prefix = "› "
				}
				body = append(body, prefix+choice.Label)
			}
		}
		if err := ui.render(view, body, "↑↓ 选择 · Tab 补全 · Enter 执行 · Esc 收起 · Ctrl+C 退出"); err != nil {
			return "", err
		}
		key, err := ui.read(ctx)
		if err != nil {
			return "", err
		}
		if ui.paging(key) {
			continue
		}
		switch key.Name {
		case "interrupt", "eof":
			return "/exit", nil
		case "escape":
			hidden = true
		case "up":
			selected = max(0, selected-1)
		case "down":
			selected = min(max(0, len(matches)-1), selected+1)
		case "tab":
			if len(matches) > 0 {
				input = []rune(matches[selected].Value)
				cursor, selected, hidden = len(input), 0, true
			}
		case "enter":
			if len(matches) > 0 && !hidden {
				return matches[selected].Value, nil
			}
			return string(input), nil
		case "left":
			cursor = max(0, cursor-1)
		case "right":
			cursor = min(len(input), cursor+1)
		case "home":
			cursor = 0
		case "end":
			cursor = len(input)
		case "clear":
			input, cursor = nil, 0
		case "backspace":
			if cursor > 0 {
				input = append(input[:cursor-1], input[cursor:]...)
				cursor--
			}
			hidden, selected = false, 0
		case "delete":
			if cursor < len(input) {
				input = append(input[:cursor], input[cursor+1:]...)
			}
			hidden, selected = false, 0
		default:
			if key.Text != "" && len(input)+len([]rune(key.Text)) <= 512 {
				insert := []rune(key.Text)
				input = append(input[:cursor], append(insert, input[cursor:]...)...)
				cursor += len(insert)
				hidden, selected = false, 0
			}
		}
	}
}

func (ui *authTerminalUI) Choose(ctx context.Context, view authruntime.ConsoleView, choices []authruntime.ConsoleChoice) (string, error) {
	ui.scroll = 0
	ui.follow = true
	selected := 0
	for {
		body := append([]string(nil), view.Details...)
		body = append(body, "")
		for i, choice := range choices {
			prefix := "  "
			if i == selected {
				prefix = "› "
			}
			body = append(body, prefix+choice.Label)
		}
		if err := ui.render(view, body, "↑↓ 选择 · Enter 确认 · Esc / Ctrl+C 取消"); err != nil {
			return "", err
		}
		key, err := ui.read(ctx)
		if err != nil {
			return "", err
		}
		if ui.paging(key) {
			continue
		}
		switch key.Name {
		case "escape", "interrupt", "eof":
			return "", nil
		case "up":
			selected = max(0, selected-1)
		case "down":
			selected = min(len(choices)-1, selected+1)
		case "enter":
			if len(choices) > 0 {
				return choices[selected].Value, nil
			}
		}
	}
}

func (ui *authTerminalUI) Wait(ctx context.Context, view authruntime.ConsoleView, notices <-chan authruntime.LoginNotice) (string, error) {
	ui.scroll = 0
	url, code, message := "", "", "正在连接授权服务…"
	for {
		select {
		case notice, ok := <-notices:
			if !ok {
				return "", nil
			}
			if notice.URL != "" {
				url, code = notice.URL, notice.Code
			}
			if notice.Message != "" {
				message = notice.Message
			}
		default:
		}
		body := []string{message}
		if url != "" {
			body = append(body, "", "授权地址（可手动打开）：", url)
		}
		if code != "" {
			body = append(body, "", "在浏览器输入设备码："+code)
		}
		body = append(body, "", "授权等待最多 15 分钟。", "Enter 切换授权方式 · Esc / Ctrl+C 取消")
		if err := ui.render(view, body, "等待授权中 · 完成后自动返回首页"); err != nil {
			return "", err
		}
		key, err := ui.read(ctx)
		if err != nil {
			return "", err
		}
		if ui.paging(key) {
			continue
		}
		switch key.Name {
		case "escape", "interrupt", "eof":
			return "cancel", nil
		case "enter":
			return "method", nil
		}
	}
}

func (ui *authTerminalUI) render(view authruntime.ConsoleView, body []string, footer string) error {
	width, height := ui.terminal.Size()
	if width < 24 || height < 10 {
		return ui.writeFrame("终端过小，请放大窗口。", width, height)
	}
	status := "未登录"
	if view.Status.LoggedIn {
		status = "已登录（本地） · 支持自动续期"
		if view.Status.NeedsRefresh {
			status = "已登录（本地） · 下次调用自动续期"
		}
	}
	if view.StatusError != "" {
		status = "凭证读取失败"
	}
	lines := []string{"SN Auth  ·  " + ui.home, "", "ChatGPT / Codex  ·  " + status}
	if view.Status.AccountFingerprint != "" {
		lines = append(lines, "账号指纹  "+view.Status.AccountFingerprint)
	}
	if view.StatusError != "" {
		lines = append(lines, view.StatusError, "凭证文件："+ui.authFile)
	}
	if view.Message != "" {
		lines = append(lines, "", view.Message)
	}
	if view.Title != "" {
		lines = append(lines, "", view.Title)
	}
	lines = append(lines, "")
	lines = append(lines, body...)
	wrapped := wrapAuthLines(lines, width-1)
	pageSize := height - 3
	if ui.follow {
		focus := -1
		for i, line := range wrapped {
			if strings.HasPrefix(line, "› ") || strings.ContainsRune(line, '▏') {
				focus = i
			}
		}
		if focus >= 0 {
			if focus < ui.scroll {
				ui.scroll = focus
			}
			if focus >= ui.scroll+pageSize {
				ui.scroll = focus - pageSize + 1
			}
		}
	}
	ui.scroll = min(ui.scroll, max(0, len(wrapped)-pageSize))
	end := min(len(wrapped), ui.scroll+pageSize)
	visible := append([]string(nil), wrapped[ui.scroll:end]...)
	for len(visible) < pageSize {
		visible = append(visible, "")
	}
	if len(wrapped) > pageSize {
		visible = append(visible, fmt.Sprintf("PgUp / PgDn 查看更多  %d–%d/%d", ui.scroll+1, end, len(wrapped)))
	} else {
		visible = append(visible, "")
	}
	visible = append(visible, wrapAuthLines([]string{footer}, width-1)[0])
	return ui.writeFrame(strings.Join(visible, "\r\n"), width, height)
}

func (ui *authTerminalUI) writeFrame(frame string, width, height int) error {
	key := fmt.Sprintf("%dx%d:%s", width, height, frame)
	if ui.last == key {
		return nil
	}
	_, err := io.WriteString(ui.output, "\x1b[H\x1b[2J"+frame)
	if err == nil {
		ui.last = key
	}
	return err
}

// Strip controls from all external text before terminal rendering. CJK and
// combining marks use display cells so wrapping cannot corrupt the screen.
func wrapAuthLines(lines []string, width int) []string {
	width = max(width, 1)
	var result []string
	for _, line := range lines {
		var current strings.Builder
		cells := 0
		for _, r := range line {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				continue
			}
			w := 1
			if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
				w = 0
			} else if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || r >= 0xff01 && r <= 0xff60 || r >= 0x1f300 && r <= 0x1faff || r >= 0x3000 && r <= 0x303f {
				w = 2
			}
			if cells+w > width && current.Len() > 0 {
				result = append(result, current.String())
				current.Reset()
				cells = 0
			}
			current.WriteRune(r)
			cells += w
		}
		result = append(result, current.String())
	}
	return result
}
