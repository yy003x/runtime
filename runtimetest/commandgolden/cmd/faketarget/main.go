package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type capture struct {
	Argv    []string          `json:"argv"`
	Env     map[string]string `json:"env"`
	Missing []string          `json:"missing,omitempty"`
	TTY     ttyState          `json:"tty"`
	Stdin   string            `json:"stdin,omitempty"`
	Signals []string          `json:"signals,omitempty"`
}

type ttyState struct {
	Stdin  bool `json:"stdin"`
	Stdout bool `json:"stdout"`
	Stderr bool `json:"stderr"`
}

func main() {
	output := strings.TrimSpace(os.Getenv("RUNTIME_GOLDEN_CAPTURE"))
	if output == "" {
		fmt.Fprintln(os.Stderr, "RUNTIME_GOLDEN_CAPTURE is required")
		os.Exit(2)
	}
	value := capture{
		Argv: append([]string(nil), os.Args[1:]...),
		Env:  map[string]string{},
		TTY: ttyState{
			Stdin:  isTerminal(os.Stdin.Fd()),
			Stdout: isTerminal(os.Stdout.Fd()),
			Stderr: isTerminal(os.Stderr.Fd()),
		},
	}
	for _, name := range strings.Split(os.Getenv("RUNTIME_GOLDEN_ENV_KEYS"), ",") {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if current, ok := os.LookupEnv(name); ok {
			value.Env[name] = current
		} else {
			value.Missing = append(value.Missing, name)
		}
	}
	sort.Strings(value.Missing)

	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)
	if os.Getenv("RUNTIME_GOLDEN_WAIT_SIGNAL") == "1" {
		ready := os.Getenv("RUNTIME_GOLDEN_READY")
		if ready != "" {
			if err := os.WriteFile(ready, []byte("ready\n"), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, err)
				os.Exit(2)
			}
		}
		received := <-signals
		value.Signals = append(value.Signals, received.String())
		if err := writeCapture(output, value); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		os.Exit(130)
	}

	switch os.Getenv("RUNTIME_GOLDEN_READ_STDIN") {
	case "line":
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && err != io.EOF {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		value.Stdin = line
	case "all":
		input, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		value.Stdin = string(input)
	}
	if err := writeCapture(output, value); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if text := os.Getenv("RUNTIME_GOLDEN_STDOUT"); text != "" {
		fmt.Fprint(os.Stdout, text)
	}
	if text := os.Getenv("RUNTIME_GOLDEN_STDERR"); text != "" {
		fmt.Fprint(os.Stderr, text)
	}
	exitCode, err := strconv.Atoi(defaultValue(os.Getenv("RUNTIME_GOLDEN_EXIT_CODE"), "0"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(exitCode)
}

func writeCapture(path string, value capture) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func defaultValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
