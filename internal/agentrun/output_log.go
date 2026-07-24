package agentrun

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/yy003x/runtime/internal/provider"
)

const outputStreamMarker = "--- stream ---\n"

func initializeOutputLog(path string, prepared provider.PreparedRequest) error {
	var builder strings.Builder
	if prepared.CLI != nil {
		fmt.Fprintf(&builder, "argv=%q\n", prepared.CLI.Argv)
	}
	builder.WriteString("running\n")
	builder.WriteString(outputStreamMarker)
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}

func appendOutputLogHeader(path string, prepared provider.PreparedRequest, phase string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if prepared.CLI != nil {
		if _, err := fmt.Fprintf(file, "argv=%q\n", prepared.CLI.Argv); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(file, "%s\n%s", phase, outputStreamMarker); err != nil {
		return err
	}
	return nil
}

func finalizeOutputLog(path string, sink *runProviderSink, result provider.Result) error {
	sink.stream.Lock()
	defer sink.stream.Unlock()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if !sink.stdout && result.Stdout != "" {
		if err := writeStreamRecord(file, "stdout", []byte(result.Stdout)); err != nil {
			return err
		}
	}
	if !sink.stderr && result.Stderr != "" {
		if err := writeStreamRecord(file, "stderr", []byte(result.Stderr)); err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(file, "[runtime] returncode=%d\n", result.ExitCode)
	return err
}

func writeStreamRecord(writer io.Writer, name string, value []byte) error {
	for _, line := range strings.SplitAfter(string(value), "\n") {
		if line == "" {
			continue
		}
		if _, err := fmt.Fprintf(writer, "[%s] %s", name, line); err != nil {
			return err
		}
	}
	return nil
}
