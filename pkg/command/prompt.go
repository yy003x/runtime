package command

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

func IsTerminal(file *os.File) bool {
	return file != nil && isTerminalFD(int(file.Fd()))
}

// ResolvePrompt applies the Profile file-or-text rule relative to the
// ingress-captured invocation base.
func ResolvePrompt(value, invocationBase string) (string, error) {
	result, err := resolvePrompt(value, invocationBase)
	if err != nil {
		return "", typedBuildError(err)
	}
	return result, nil
}

func resolvePrompt(value, invocationBase string) (string, error) {
	if value == "" {
		return "", nil
	}
	if len(value) > MaxTokenBytes {
		return "", &invocationLimitError{message: fmt.Sprintf(
			"prompt exceeds %d bytes", MaxTokenBytes,
		)}
	}
	if err := validateTextToken("prompt", value, MaxTokenBytes, true); err != nil {
		return "", err
	}
	if invocationBase == "" || !filepath.IsAbs(invocationBase) {
		return "", fmt.Errorf("invocation base must be an absolute path")
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(invocationBase, path)
	}
	before, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return value, nil
		}
		return "", fmt.Errorf("inspect prompt file %q: %w", path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("prompt file %q must not be a symlink", path)
	}
	file, err := openPromptFileNoFollow(path)
	if err != nil {
		return "", fmt.Errorf("open prompt file %q: %w", path, err)
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("stat prompt file %q: %w", path, err)
	}
	if !after.Mode().IsRegular() {
		return "", fmt.Errorf("prompt file %q must be a regular file", path)
	}
	if !os.SameFile(before, after) {
		return "", fmt.Errorf("prompt file %q changed while opening", path)
	}
	content, err := io.ReadAll(io.LimitReader(file, MaxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("read prompt file %q: %w", path, err)
	}
	if len(content) > MaxTokenBytes {
		return "", &invocationLimitError{message: fmt.Sprintf(
			"prompt file %q exceeds %d bytes", path, MaxTokenBytes,
		)}
	}
	result := string(content)
	if err := validateTextToken("prompt file", result, MaxTokenBytes, true); err != nil {
		return "", err
	}
	return result, nil
}

func ReadPrompt(reader io.Reader) (string, error) {
	result, err := readPrompt(reader)
	if err != nil {
		return "", typedBuildError(err)
	}
	return result, nil
}

func readPrompt(reader io.Reader) (string, error) {
	if reader == nil {
		return "", nil
	}
	content, err := io.ReadAll(io.LimitReader(reader, MaxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("read prompt input: %w", err)
	}
	if len(content) > MaxTokenBytes {
		return "", &invocationLimitError{message: fmt.Sprintf(
			"prompt input exceeds %d bytes", MaxTokenBytes,
		)}
	}
	result := string(content)
	if err := validateTextToken("prompt input", result, MaxTokenBytes, true); err != nil {
		return "", err
	}
	return result, nil
}

func MergePrompt(fragments ...string) (string, error) {
	result, err := mergePrompt(fragments...)
	if err != nil {
		return "", typedBuildError(err)
	}
	return result, nil
}

func mergePrompt(fragments ...string) (string, error) {
	nonempty := make([]string, 0, len(fragments))
	total := 0
	for _, fragment := range fragments {
		if fragment == "" {
			continue
		}
		if len(fragment) > MaxTokenBytes {
			return "", &invocationLimitError{message: fmt.Sprintf(
				"prompt fragment exceeds %d bytes", MaxTokenBytes,
			)}
		}
		if err := validateTextToken(
			"prompt fragment", fragment, MaxTokenBytes, true,
		); err != nil {
			return "", err
		}
		total += len(fragment)
		if len(nonempty) > 0 {
			total++
		}
		if total > MaxTokenBytes {
			return "", &invocationLimitError{message: fmt.Sprintf(
				"merged prompt exceeds %d bytes", MaxTokenBytes,
			)}
		}
		nonempty = append(nonempty, fragment)
	}
	return strings.Join(nonempty, "\n"), nil
}
