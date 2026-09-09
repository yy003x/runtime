package authruntime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/infrastructure/codexauth"
)

// Console is an ephemeral authentication use case, not a Runtime Session/Run.
// UI implementations own only rendering and editing, never credential state.
type Console struct {
	Service     *Service
	UI          ConsoleUI
	OpenBrowser func(context.Context, string) error
}

type ConsoleView struct {
	Status      Status
	StatusError string
	Message     string
	Title       string
	Details     []string
}

type ConsoleChoice struct {
	Value string
	Label string
}

type LoginNotice struct {
	URL     string
	Code    string
	Message string
}

type ConsoleUI interface {
	Command(context.Context, ConsoleView) (string, error)
	Choose(context.Context, ConsoleView, []ConsoleChoice) (string, error)
	Wait(context.Context, ConsoleView, <-chan LoginNotice) (string, error)
}

type ConsoleCommand struct {
	Name        string
	Description string
	Provider    bool
}

// Commands is also the completion/help inventory; no parallel command registry.
func ConsoleCommands() []ConsoleCommand {
	return []ConsoleCommand{
		{Name: "/login", Description: "登录 / 重新授权", Provider: true},
		{Name: "/logout", Description: "退出登录 · 清除本地凭证", Provider: true},
		{Name: "/status", Description: "查看本地认证状态", Provider: true},
		{Name: "/help", Description: "命令与快捷键"},
		{Name: "/exit", Description: "退出界面 · 保留登录"},
	}
}

func (console *Console) view(message string) (ConsoleView, uint64) {
	status, revision, err := console.Service.snapshot()
	view := ConsoleView{Status: status, Message: message}
	if err != nil {
		view.StatusError = err.Error()
	}
	return view, revision
}

func (console *Console) Run(ctx context.Context, initial string) error {
	message, command := "", initial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		view, revision := console.view(message)
		if command == "" {
			var err error
			command, err = console.UI.Command(ctx, view)
			if err != nil {
				return err
			}
			// Always re-read after user input, not only when the screen was opened.
			view, revision = console.view("")
		}
		fields := strings.Fields(command)
		command, message = "", ""
		if len(fields) == 0 {
			continue
		}
		valid := false
		for _, spec := range ConsoleCommands() {
			if fields[0] == spec.Name && (len(fields) == 1 || spec.Provider && len(fields) == 2 && fields[1] == codexauth.Provider) {
				valid = true
				break
			}
		}
		if !valid {
			// Never echo arbitrary pasted input, tokens, or unknown arguments.
			message = "无法识别命令或参数。输入 / 查看命令；服务 ID 为 openai-codex。"
			continue
		}
		switch fields[0] {
		case "/exit":
			return nil
		case "/help":
			view.Title = "帮助"
			for _, spec := range ConsoleCommands() {
				view.Details = append(view.Details, spec.Name+"  "+spec.Description)
			}
			view.Details = append(view.Details,
				"↑↓ 选择 · Tab 补全 · Enter 执行 · Esc 返回 / 取消",
				"Ctrl+C：取消操作；在首页退出界面，不退出登录。",
				"授权由你在浏览器完成；本地状态不代表远端权限或额度。",
				"凭证属于当前 Runtime home；不读取或修改 Pi / Codex CLI 凭证。")
			if _, err := console.UI.Choose(ctx, view, []ConsoleChoice{{Value: "back", Label: "返回"}}); err != nil {
				return err
			}
		case "/status":
			message = "已重新读取本地状态；未刷新凭证、未查询远端权限或额度。"
			if view.Status.ExpiresAt != nil {
				message += " 访问令牌到期：" + view.Status.ExpiresAt.Local().Format("2006-01-02 15:04:05 MST") + "；调用时按需自动续期，不代表届时必须重新登录。"
			}
		case "/login":
			if view.StatusError != "" {
				message = "凭证读取失败，未开始登录。请检查下方错误与凭证文件权限；不会自动删除文件。"
				continue
			}
			var err error
			message, err = console.login(ctx, view, revision)
			if err != nil {
				return err
			}
		case "/logout":
			var err error
			message, err = console.logout(ctx)
			if err != nil {
				return err
			}
		}
	}
}

func (console *Console) logout(ctx context.Context) (string, error) {
	message := ""
	for {
		view, revision := console.view(message)
		if view.StatusError != "" {
			return "凭证读取失败，未退出登录；不会自动删除文件。", nil
		}
		if !view.Status.LoggedIn {
			return "当前没有已登录服务。输入 /login 开始登录。", nil
		}
		view.Title = "退出 ChatGPT / Codex 登录？"
		view.Details = []string{
			"将清除当前 Runtime home 的本地凭证，后续调用需要重新登录。",
			"不影响浏览器、Pi 或 Codex CLI，不撤销远端 token 或已发出的请求。",
		}
		choice, err := console.UI.Choose(ctx, view, []ConsoleChoice{{Value: "cancel", Label: "取消"}, {Value: "logout", Label: "退出登录"}})
		if err != nil || choice != "logout" {
			return "已取消退出登录。", err
		}
		opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		err = console.Service.logout(opCtx, &revision)
		cancel()
		if errors.Is(err, errCredentialsChanged) {
			message = "确认期间凭证已变化。请核对当前账号，重新确认。"
			continue
		}
		if err != nil {
			return "退出登录失败：" + err.Error(), nil
		}
		view, _ = console.view("")
		if view.StatusError != "" || view.Status.LoggedIn {
			return "退出操作已完成，但当前凭证状态已变化或无法读取；请查看 /status。", nil
		}
		return "已退出 ChatGPT / Codex 登录。本地凭证已清除。", nil
	}
}

func (console *Console) login(ctx context.Context, view ConsoleView, revision uint64) (string, error) {
	if view.Status.LoggedIn {
		view.Title = "当前已有登录，是否重新授权？"
		view.Details = []string{"需要更换账号时，请在浏览器中选择新账号。", "新凭证成功保存后才替换原凭证；失败或取消不会先清空账号。"}
		choice, err := console.UI.Choose(ctx, view, []ConsoleChoice{{Value: "cancel", Label: "取消"}, {Value: "login", Label: "重新授权"}})
		if err != nil || choice != "login" {
			return "已取消重新授权。", err
		}
	}
	for {
		view.Title = "选择登录方式"
		view.Details = []string{"请由你本人完成浏览器中的登录、验证码和授权。"}
		method, err := console.UI.Choose(ctx, view, []ConsoleChoice{{Value: "browser", Label: "浏览器授权（推荐）"}, {Value: "device", Label: "设备码授权（远程终端）"}, {Value: "cancel", Label: "返回"}})
		if err != nil || method != "browser" && method != "device" {
			return "已取消登录；原凭证未清除。", err
		}
		for {
			action, result, err := console.authorize(ctx, view, revision, method == "device")
			if err != nil {
				return "", err
			}
			// Completion wins a simultaneous cancel: never claim an already
			// committed login was cancelled or start a second authorization.
			if result.err == nil {
				current, _ := console.view("")
				if current.StatusError != "" || !current.Status.LoggedIn || current.Status.AccountFingerprint != result.status.AccountFingerprint {
					return "登录已完成，但当前凭证已变化或无法读取；请查看 /status。", nil
				}
				return "登录成功，凭证已保存。退出界面后可运行：sn-cli call api-codex \"你好\"", nil
			}
			if errors.Is(result.err, errCredentialsChanged) {
				return "凭证已被其他操作更新，未覆盖。请重新输入 /login 核对当前账号。", nil
			}
			if action == "method" {
				break
			}
			if action == "cancel" || errors.Is(result.err, context.Canceled) {
				return "已取消登录；原凭证未清除。", nil
			}
			view.Title = "登录未完成"
			view.Details = []string{result.err.Error(), "原凭证未清除；不会自动重试授权。"}
			if errors.Is(result.err, context.DeadlineExceeded) {
				view.Details[0] = "授权等待超时，请重试或切换授权方式。"
			}
			choice, err := console.UI.Choose(ctx, view, []ConsoleChoice{{Value: "back", Label: "返回首页"}, {Value: "retry", Label: "重试"}, {Value: "method", Label: "切换授权方式"}})
			if err != nil || choice == "back" || choice == "" {
				return "登录未完成；原凭证未清除。", err
			}
			if choice == "method" {
				break
			}
		}
	}
}

type loginResult struct {
	status Status
	err    error
}

func (console *Console) authorize(ctx context.Context, view ConsoleView, revision uint64, device bool) (string, loginResult, error) {
	opCtx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	notices := make(chan LoginNotice, 4)
	completed := make(chan loginResult, 1)
	publish := func(notice LoginNotice) error {
		select {
		case notices <- notice:
			return nil
		case <-opCtx.Done():
			return opCtx.Err()
		}
	}
	go func() {
		status, err := console.Service.login(opCtx, device, func(url, code string) error {
			if err := publish(LoginNotice{URL: url, Code: code, Message: "等待你在浏览器完成授权…"}); err != nil {
				return err
			}
			if !device && console.OpenBrowser != nil {
				if err := console.OpenBrowser(opCtx, url); err != nil {
					return publish(LoginNotice{Message: "无法自动打开浏览器。请手动打开上方地址，或切换设备码授权。"})
				}
			}
			return nil
		}, &revision)
		completed <- loginResult{status: status, err: err}
		close(notices)
	}()
	view.Title = "正在准备浏览器授权…"
	if device {
		view.Title = "正在准备设备码授权…"
	}
	view.Message, view.Details = "", nil
	action, err := console.UI.Wait(opCtx, view, notices)
	cancel()
	result := <-completed // Join before changing method, logging out, or exiting.
	if ctx.Err() != nil {
		return "", result, ctx.Err()
	}
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return "", result, fmt.Errorf("authentication console: %w", err)
	}
	return action, result, nil
}
