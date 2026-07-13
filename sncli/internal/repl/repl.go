package repl

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"agent-arch/sncli/internal/config"
	"agent-arch/sncli/internal/native"
	"agent-arch/sncli/internal/runtime"
	"agent-arch/sncli/internal/session"
)

type App struct {
	Config  *config.Config
	Store   session.Store
	Runtime runtime.Client
}

func (a App) Start(existing *session.Session) error {
	cwd, _ := os.Getwd()
	current := existing
	var err error
	if current == nil {
		current, err = a.Store.New(a.Config.DefaultProvider, cwd)
		if err != nil {
			return err
		}
	}
	fmt.Printf("sn-cli session %s provider=%s\n", current.ID, current.Provider)
	fmt.Println("type /help for commands")
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Print("sn> ")
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				fmt.Println()
				return nil
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "/") {
			done, err := a.command(current, line)
			if err != nil {
				fmt.Fprintf(os.Stderr, "error: %v\n", err)
			}
			if done {
				return nil
			}
			continue
		}
		if err := a.runPrompt(current, line); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		}
	}
}

func (a App) command(current *session.Session, line string) (bool, error) {
	fields := strings.Fields(line)
	switch fields[0] {
	case "/exit", "/quit":
		return true, nil
	case "/help":
		printHelp()
	case "/provider":
		if len(fields) != 2 {
			return false, fmt.Errorf("usage: /provider codex|claude|fake")
		}
		current.Provider = fields[1]
		if err := a.Store.Save(current); err != nil {
			return false, err
		}
		fmt.Printf("provider=%s\n", current.Provider)
	case "/runtime":
		prompt := strings.TrimSpace(strings.TrimPrefix(line, "/runtime"))
		if prompt == "" {
			return false, fmt.Errorf("usage: /runtime <prompt>")
		}
		return false, a.runPrompt(current, prompt)
	case "/native":
		provider := current.Provider
		if len(fields) > 1 {
			provider = fields[1]
		}
		return false, a.openNative(provider)
	case "/session":
		fmt.Printf("id=%s provider=%s cwd=%s\n", current.ID, current.Provider, current.CWD)
	case "/logs":
		fmt.Printf("messages: %s/messages.jsonl\n", a.Store.Root+"/"+current.ID)
	default:
		return false, fmt.Errorf("unknown command: %s", fields[0])
	}
	return false, nil
}

func (a App) runPrompt(current *session.Session, prompt string) error {
	if err := a.Store.Append(current.ID, session.Message{Role: "user", Provider: current.Provider, Text: prompt}); err != nil {
		return err
	}
	result, err := a.Runtime.Run(runtime.RunOptions{
		Provider: current.Provider,
		Prompt:   prompt,
		CWD:      current.CWD,
		Sandbox:  a.Config.Runtime.DefaultSandbox,
		Timeout:  a.Config.Runtime.DefaultTimeoutSeconds,
	})
	if err != nil {
		return err
	}
	fmt.Println(result.FinalText)
	if runDir := result.Artifacts["run_dir"]; runDir != "" {
		fmt.Printf("[run:%s] %s\n", result.RunID, runDir)
	}
	return a.Store.Append(current.ID, session.Message{Role: "assistant", Provider: result.Provider, RunID: result.RunID, Text: result.FinalText})
}

func (a App) openNative(providerName string) error {
	if a.Config.NativeProfile(providerName) == "" {
		return fmt.Errorf("unknown native provider: %s", providerName)
	}
	cwd, _ := os.Getwd()
	return native.Open(a.Config, providerName, cwd)
}

func printHelp() {
	fmt.Println(`/provider codex|claude|fake
/runtime <prompt>
/native codex|claude
/session
/logs
/help
/exit`)
}
