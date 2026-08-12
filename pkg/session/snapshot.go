package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/profile"
	"golang.org/x/sys/unix"
)

const maxBasePromptBytes = runtimecommand.MaxTokenBytes

func (service *Service) PrepareRunRequest(
	request RunRequest,
) (RunRequest, *contract.RuntimeError) {
	entry, exists := service.profiles.Resolve(request.ProfileID)
	if !exists {
		return RunRequest{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			fmt.Sprintf("unknown profile %q", request.ProfileID),
		)
	}
	if err := validateRunRequest(request, entry); err != nil {
		return RunRequest{}, sessionRuntimeError(
			contract.ErrorInvalidRequest, err.Error(),
		)
	}
	return service.prepareRunRequest(request, entry)
}

func (service *Service) prepareRunRequest(
	request RunRequest,
	entry profile.Entry,
) (RunRequest, *contract.RuntimeError) {
	configDigest, err := profileConfigDigest(entry)
	if err != nil {
		return RunRequest{}, sessionRuntimeError(
			contract.ErrorInternal, "encode Profile snapshot",
		)
	}
	request.preparedConfigDigest = configDigest
	if entry.Kind == profile.KindModel {
		if request.Snapshot != nil {
			return RunRequest{}, sessionRuntimeError(
				contract.ErrorInvalidRequest,
				"private CLI snapshot is invalid for an API Profile",
			)
		}
		request.preparedRequestDigest = computeRequestDigest(
			request, configDigest, "",
		)
		return request, nil
	}
	if request.Snapshot != nil {
		snapshot := request.Snapshot
		if snapshot.SchemaVersion != SchemaVersion ||
			snapshot.ProfileID != entry.ID {
			return RunRequest{}, sessionRuntimeError(
				contract.ErrorConflict,
				"private CLI snapshot does not match the Session request",
			)
		}
		if snapshot.ConfigDigest != configDigest {
			return RunRequest{}, sessionRuntimeError(
				contract.ErrorConflict,
				"CLI Profile changed after the durable Run was submitted",
			)
		}
		if request.Model != snapshot.Model ||
			request.Effort != snapshot.Effort ||
			request.CWD != snapshot.CWD {
			return RunRequest{}, sessionRuntimeError(
				contract.ErrorConflict,
				"public Session request does not match its private snapshot",
			)
		}
		request.preparedBasePromptDigest = snapshot.BasePromptDigest
		request.preparedRequestDigest = computeRequestDigest(
			request, configDigest, snapshot.BasePromptDigest,
		)
		if snapshot.RequestDigest != request.preparedRequestDigest {
			return RunRequest{}, sessionRuntimeError(
				contract.ErrorConflict,
				"private CLI snapshot request digest does not match",
			)
		}
		return request, nil
	}
	if request.InvocationBase == "" &&
		request.CWD != "" &&
		!filepath.IsAbs(request.CWD) {
		return RunRequest{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"CLI Session cwd override must be absolute outside CLI ingress",
		)
	}
	base := filepath.Clean(request.InvocationBase)
	if request.InvocationBase == "" || !filepath.IsAbs(base) {
		switch {
		case filepath.IsAbs(request.CWD):
			base = filepath.Clean(request.CWD)
		case filepath.IsAbs(entry.Command.CWD):
			base = filepath.Clean(entry.Command.CWD)
		default:
			return RunRequest{}, sessionRuntimeError(
				contract.ErrorInvalidRequest,
				"CLI Session requires an absolute invocation base or cwd",
			)
		}
	}
	basePrompt, err := resolvePromptFragment(entry.Command.Prompt, base)
	if err != nil {
		var runtimeErr *contract.RuntimeError
		if errors.As(err, &runtimeErr) {
			return RunRequest{}, runtimeErr
		}
		return RunRequest{}, sessionRuntimeError(
			contract.ErrorInvalidRequest, err.Error(),
		)
	}
	if request.Model != "" {
		if len(request.Model) > runtimecommand.MaxTokenBytes ||
			!utf8.ValidString(request.Model) ||
			strings.ContainsRune(request.Model, '\x00') {
			return RunRequest{}, sessionRuntimeError(
				contract.ErrorInvalidRequest,
				"model must be a UTF-8 token within the configured limit",
			)
		}
	}
	if request.Effort != "" {
		_, err := runtimecommand.ParseEffort(request.Effort)
		if err != nil {
			return RunRequest{}, sessionRuntimeError(
				contract.ErrorInvalidRequest, err.Error(),
			)
		}
	}
	var cwdOverride *string
	if request.CWD != "" {
		value := request.CWD
		cwdOverride = &value
	}
	rawCWD := entry.Command.CWD
	if cwdOverride != nil {
		rawCWD = *cwdOverride
	}
	resolvedCWD, err := resolveSnapshotCWD(rawCWD, base, service.environ)
	if err != nil {
		return RunRequest{}, sessionRuntimeError(
			contract.ErrorInvalidRequest, err.Error(),
		)
	}
	request.CWD = resolvedCWD
	if request.Model == "" {
		request.Model = entry.Command.Model
	}
	if request.Effort == "" {
		request.Effort = string(entry.Command.Effort)
	}
	basePromptDigest := ""
	if basePrompt != "" {
		basePromptDigest = digest([]byte(basePrompt))
	}
	request.preparedBasePromptDigest = basePromptDigest
	request.preparedRequestDigest = computeRequestDigest(
		request, configDigest, basePromptDigest,
	)
	request.Snapshot = &CLIExecutionSnapshot{
		SchemaVersion: SchemaVersion, ProfileID: entry.ID,
		Profile: *entry.Command, BasePrompt: basePrompt,
		ConfigDigest: configDigest, BasePromptDigest: basePromptDigest,
		RequestDigest: request.preparedRequestDigest,
		CWD:           request.CWD, Model: request.Model, Effort: request.Effort,
	}
	return request, nil
}

func resolveSnapshotCWD(
	raw, invocationBase string,
	environ []string,
) (string, error) {
	values := make(map[string]string, len(environ))
	for _, item := range environ {
		name, value, exists := strings.Cut(item, "=")
		if exists {
			values[name] = value
		}
	}
	resolved, err := expandSnapshotReferences(raw, values)
	if err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	if !utf8.ValidString(resolved) || strings.ContainsRune(resolved, '\x00') {
		return "", fmt.Errorf("cwd must be UTF-8 without NUL")
	}
	cwd := invocationBase
	if resolved != "" {
		if filepath.IsAbs(resolved) {
			cwd = filepath.Clean(resolved)
		} else {
			cwd = filepath.Clean(filepath.Join(invocationBase, resolved))
		}
	}
	if !utf8.ValidString(cwd) || strings.ContainsRune(cwd, '\x00') {
		return "", fmt.Errorf("cwd must be UTF-8 without NUL")
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cwd is not a directory")
	}
	if err := unix.Access(cwd, unix.X_OK); err != nil {
		return "", fmt.Errorf("cwd is not enterable: %w", err)
	}
	return cwd, nil
}

func expandSnapshotReferences(
	value string,
	environ map[string]string,
) (string, error) {
	var result strings.Builder
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			result.WriteString(value)
			return result.String(), nil
		}
		result.WriteString(value[:start])
		remainder := value[start+2:]
		end := strings.IndexByte(remainder, '}')
		if end < 0 {
			return "", fmt.Errorf("environment reference is missing }")
		}
		name := remainder[:end]
		replacement, exists := environ[name]
		if !exists {
			return "", fmt.Errorf("environment variable is not set: %s", name)
		}
		result.WriteString(replacement)
		value = remainder[end+1:]
	}
}

func (request RunRequest) SnapshotDigest() string {
	if request.preparedRequestDigest != "" {
		return request.preparedRequestDigest
	}
	if request.Snapshot != nil {
		return request.Snapshot.RequestDigest
	}
	return ""
}

func (request RunRequest) ConfigDigest() string {
	if request.preparedConfigDigest != "" {
		return request.preparedConfigDigest
	}
	if request.Snapshot != nil {
		return request.Snapshot.ConfigDigest
	}
	return ""
}

func (request RunRequest) BasePromptDigest() string {
	if request.preparedBasePromptDigest != "" {
		return request.preparedBasePromptDigest
	}
	if request.Snapshot != nil {
		return request.Snapshot.BasePromptDigest
	}
	return ""
}

func requestDigest(request RunRequest) string {
	return request.SnapshotDigest()
}

func requestConfigDigest(request RunRequest, entry profile.Entry) string {
	if value := request.ConfigDigest(); value != "" {
		return value
	}
	value, _ := profileConfigDigest(entry)
	return value
}

func requestBasePromptDigest(request RunRequest) string {
	return request.BasePromptDigest()
}

func profileConfigDigest(entry profile.Entry) (string, error) {
	value := any(entry.Command)
	if entry.Kind == profile.KindModel {
		value = entry.Model
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return digest(data), nil
}

func computeRequestDigest(
	request RunRequest,
	configDigest, basePromptDigest string,
) string {
	value := struct {
		ProfileID        string                   `json:"profile_id"`
		Input            string                   `json:"input"`
		Model            string                   `json:"model,omitempty"`
		Effort           string                   `json:"effort,omitempty"`
		CWD              string                   `json:"cwd,omitempty"`
		ModelOptions     contract.GenerateOptions `json:"model_options,omitempty"`
		ConfigDigest     string                   `json:"config_digest"`
		BasePromptDigest string                   `json:"base_prompt_digest,omitempty"`
	}{
		ProfileID: request.ProfileID, Input: request.Input,
		Model: request.Model, Effort: request.Effort, CWD: request.CWD,
		ModelOptions: request.ModelOptions, ConfigDigest: configDigest,
		BasePromptDigest: basePromptDigest,
	}
	data, _ := json.Marshal(value)
	return digest(data)
}

func resolvePromptFragment(value, invocationBase string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("Profile prompt must be UTF-8 without NUL")
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(invocationBase, path)
	}
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if errors.Is(err, os.ErrNotExist) {
		if len(value) > maxBasePromptBytes {
			return "", fmt.Errorf(
				"Profile prompt exceeds %d bytes", maxBasePromptBytes,
			)
		}
		return value, nil
	}
	if err != nil {
		return "", fmt.Errorf("open Profile prompt: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("Profile prompt path must be a regular file")
	}
	if info.Size() > maxBasePromptBytes {
		return "", fmt.Errorf(
			"Profile prompt file exceeds %d bytes", maxBasePromptBytes,
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBasePromptBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxBasePromptBytes {
		return "", fmt.Errorf(
			"Profile prompt file exceeds %d bytes", maxBasePromptBytes,
		)
	}
	if !utf8.Valid(data) || strings.IndexByte(string(data), 0) >= 0 {
		return "", fmt.Errorf("Profile prompt file must be UTF-8 without NUL")
	}
	return string(data), nil
}
