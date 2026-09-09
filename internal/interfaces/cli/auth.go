package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
)

func runAuthNamespace(paths layout.Paths, args []string, output *cliOutput) error {
	if len(args) == 0 || len(args) == 1 && (args[0] == "login" || args[0] == "logout") {
		stdout, ok := output.stdout.(*os.File)
		if output.JSON() || !ok || !runtimecommand.IsTerminal(os.Stdin) || !runtimecommand.IsTerminal(stdout) || os.Getenv("TERM") == "dumb" {
			return cliValidationf("auth console requires an interactive terminal without --json; use sn-cli auth login|status|logout openai-codex [--device-code|--no-browser]")
		}
		initial := ""
		if len(args) == 1 {
			initial = "/" + args[0]
		}
		return runAuthConsole(paths, os.Stdin, stdout, initial)
	}
	if len(args) == 1 && args[0] == "status" {
		args = []string{"status", "openai-codex"}
	}
	if len(args) < 2 || args[1] != "openai-codex" {
		return cliValidationf("usage: auth login|status|logout openai-codex [--device-code|--no-browser]")
	}
	device, noBrowser := false, false
	if args[0] != "login" && args[0] != "status" && args[0] != "logout" {
		return cliValidationf("unknown auth action; use login, status, or logout")
	}
	for _, option := range args[2:] {
		if args[0] != "login" {
			return cliValidationf("auth %s does not accept options", args[0])
		}
		switch option {
		case "--device-code":
			if device {
				return cliValidationf("duplicate --device-code")
			}
			device = true
		case "--no-browser":
			if noBrowser {
				return cliValidationf("duplicate --no-browser")
			}
			noBrowser = true
		default:
			return cliValidationf("unsupported auth login option")
		}
	}
	if device && noBrowser {
		return cliValidationf("--device-code and --no-browser are mutually exclusive")
	}
	service := runtimebootstrap.LoadAuthService(paths)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	status, err := service.Status()
	if err != nil {
		return authError(err)
	}
	switch args[0] {
	case "login":
		status, err = service.Login(ctx, device, func(url, code string) error {
			message := "Open this URL in your browser to authorize OpenAI Codex:\n" + url
			if code != "" {
				message += "\nDevice code: " + code
			}
			if err := output.diagnostic(message); err != nil {
				return err
			}
			if !device && !noBrowser {
				if err := openAuthBrowser(ctx, url); err != nil {
					return output.diagnostic("Could not open the browser automatically. Open the URL above manually.")
				}
			}
			return nil
		})
	case "logout":
		err = service.Logout(ctx)
		if err == nil {
			status, err = service.Status()
		}
	}
	if err != nil {
		return authError(err)
	}
	if output.JSON() {
		return output.writeJSON(map[string]any{"ok": true, "action": args[0], "auth": status, "credential_file": paths.AuthFile})
	}
	if !status.LoggedIn {
		return output.line("OpenAI Codex: not logged in (local credentials).")
	}
	if args[0] == "status" {
		return output.line("OpenAI Codex: logged in locally; account %s.\nAccess token expires: %s; needs_refresh=%t. Automatically refreshed on demand before calls; this is not a login deadline.\nCredentials: %s", status.AccountFingerprint, status.ExpiresAt.Format(time.RFC3339), status.NeedsRefresh, paths.AuthFile)
	}
	return output.line("OpenAI Codex: logged in locally; account %s. Automatic token refresh is enabled for calls.\nCredentials: %s", status.AccountFingerprint, paths.AuthFile)
}

func openAuthBrowser(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	switch runtime.GOOS {
	case "darwin":
		return exec.CommandContext(ctx, "open", url).Run()
	case "linux":
		return exec.CommandContext(ctx, "xdg-open", url).Run()
	default:
		return fmt.Errorf("no browser launcher")
	}
}

func authError(err error) error {
	code := contract.ErrorAuthenticationFailed
	if errors.Is(err, context.Canceled) {
		code = contract.ErrorCancelled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		code = contract.ErrorTimeout
	}
	return &contract.RuntimeError{Code: code, Phase: contract.PhaseProfile, Message: err.Error(), Provider: "openai-codex"}
}
