// nativetuitarget 是 tmux/native TUI 集成测试使用的长期交互进程。
package main

import (
	"bufio"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	factPath := os.Getenv("SN_NATIVE_TUI_FACT")
	if factPath == "" {
		os.Exit(90)
	}
	fact, err := os.OpenFile(
		factPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600,
	)
	if err != nil {
		os.Exit(91)
	}
	defer fact.Close()
	stdin, _ := os.Stdin.Stat()
	stdout, _ := os.Stdout.Stat()
	tty := stdin != nil && stdout != nil &&
		stdin.Mode()&os.ModeCharDevice != 0 &&
		stdout.Mode()&os.ModeCharDevice != 0
	ignoreTermination := os.Getenv("SN_NATIVE_TUI_IGNORE_TERM") == "1"
	if ignoreTermination {
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
	}
	_, _ = fmt.Fprintf(
		fact,
		"tty:%t\npid:%d\nparent_pid:%d\nignore_termination:%t\nargv:",
		tty, os.Getpid(), os.Getppid(), ignoreTermination,
	)
	for _, argument := range os.Args[1:] {
		_, _ = fmt.Fprintf(fact, "<%s>", argument)
	}
	_, _ = fmt.Fprintln(fact)
	_ = fact.Sync()
	if ignoreTermination {
		for {
			time.Sleep(time.Hour)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		_, _ = fmt.Fprintf(fact, "input:%s\n", scanner.Text())
		_ = fact.Sync()
	}
}
