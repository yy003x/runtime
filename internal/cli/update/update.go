package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/activation"
	"github.com/yy003x/runtime/internal/cli/config"
	"github.com/yy003x/runtime/internal/installbundle"
)

type Status struct {
	SchemaVersion   int       `json:"schema_version"`
	Enabled         bool      `json:"enabled"`
	Repository      string    `json:"repository"`
	CurrentVersion  string    `json:"current_version"`
	LatestVersion   string    `json:"latest_version,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	CheckedAt       time.Time `json:"checked_at"`
	Message         string    `json:"message,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type ApplyResult struct {
	Version             string   `json:"version"`
	Archive             string   `json:"archive"`
	Binary              string   `json:"binary"`
	ServerBinary        string   `json:"server_binary"`
	CopiedProfiles      []string `json:"copied_profiles"`
	CopiedRuntimeConfig bool     `json:"copied_runtime_config"`
	CopiedResources     []string `json:"copied_resources"`
}

type state struct {
	CheckedAt      time.Time `json:"checked_at"`
	CurrentVersion string    `json:"current_version,omitempty"`
	LatestVersion  string    `json:"latest_version,omitempty"`
}

func Check(ctx context.Context, cfg *config.Config, currentVersion string) Status {
	status := Status{
		SchemaVersion: 1, Enabled: cfg.UpdateEnabled(), Repository: cfg.Update.Repository,
		CurrentVersion: currentVersion, CheckedAt: time.Now().UTC(),
	}
	if !status.Enabled {
		status.Message = "update check disabled"
		return status
	}
	latest, err := latestRelease(ctx, cfg)
	if err != nil {
		status.Error = err.Error()
		status.Message = "update check failed"
		_ = writeState(cfg, status)
		return status
	}
	status.LatestVersion = latest
	status.UpdateAvailable = normalizeVersion(latest) != normalizeVersion(currentVersion)
	if status.UpdateAvailable {
		status.Message = "update available"
	} else {
		status.Message = "up to date"
	}
	_ = writeState(cfg, status)
	return status
}

func Plan(cfg *config.Config, version string) (archiveName, archiveURL, checksumURL string, err error) {
	if strings.TrimSpace(version) == "" {
		return "", "", "", fmt.Errorf("release version is required")
	}
	osName, arch, err := platform()
	if err != nil {
		return "", "", "", err
	}
	archiveName = fmt.Sprintf("sn-cli-%s-%s.tar.gz", osName, arch)
	base := strings.TrimRight(os.Getenv("SN_CLI_RELEASE_BASE_URL"), "/")
	if base == "" {
		base = fmt.Sprintf("https://github.com/%s/releases/download", cfg.Update.Repository)
	}
	releaseBase := base + "/" + version
	return archiveName, releaseBase + "/" + archiveName, releaseBase + "/checksums.txt", nil
}

func Apply(ctx context.Context, cfg *config.Config, version string) (ApplyResult, error) {
	if strings.TrimSpace(version) == "" {
		latest, err := latestRelease(ctx, cfg)
		if err != nil {
			return ApplyResult{}, err
		}
		version = latest
	}
	archiveName, archiveURL, checksumURL, err := Plan(cfg, version)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := os.MkdirAll(cfg.Paths.TmpDir, 0o700); err != nil {
		return ApplyResult{}, err
	}
	temporary, err := os.MkdirTemp(cfg.Paths.TmpDir, "update-")
	if err != nil {
		return ApplyResult{}, err
	}
	defer os.RemoveAll(temporary)
	archivePath := filepath.Join(temporary, archiveName)
	checksumsPath := filepath.Join(temporary, "checksums.txt")
	client := &http.Client{Timeout: 2 * time.Minute}
	if err := download(ctx, client, archiveURL, archivePath); err != nil {
		return ApplyResult{}, err
	}
	if err := download(ctx, client, checksumURL, checksumsPath); err != nil {
		return ApplyResult{}, err
	}
	checksums, err := os.ReadFile(checksumsPath)
	if err != nil {
		return ApplyResult{}, err
	}
	if err := installbundle.VerifyChecksum(archivePath, archiveName, checksums); err != nil {
		return ApplyResult{}, err
	}
	payload := filepath.Join(temporary, "payload")
	if err := installbundle.ExtractTarGz(archivePath, payload); err != nil {
		return ApplyResult{}, err
	}
	binary := filepath.Join(payload, "sn-cli")
	serverBinary := filepath.Join(payload, "sn-server")
	packagedProfiles := filepath.Join(payload, "configs")
	packagedRuntimeConfig := filepath.Join(payload, "runtime.json")
	packagedResources := filepath.Join(payload, "resources")
	if err := validatePayload(
		binary, serverBinary, packagedProfiles, packagedRuntimeConfig,
		packagedResources,
	); err != nil {
		return ApplyResult{}, err
	}
	coordinatorPID := 0
	if current, executableErr := os.Executable(); executableErr == nil {
		if currentInfo, currentErr := os.Stat(current); currentErr == nil {
			if targetInfo, targetErr := os.Stat(cfg.Paths.Binary); targetErr == nil &&
				os.SameFile(currentInfo, targetInfo) {
				coordinatorPID = os.Getpid()
			}
		}
	}
	activated, err := runCandidateActivation(
		ctx, binary, payload, cfg.Home, coordinatorPID,
	)
	if err != nil {
		return ApplyResult{}, err
	}
	result := ApplyResult{
		Version: version, Archive: archiveName, Binary: cfg.Paths.Binary,
		ServerBinary:        cfg.Paths.ServerBinary,
		CopiedProfiles:      activated.CopiedProfiles,
		CopiedRuntimeConfig: activated.CopiedRuntimeConfig,
		CopiedResources:     activated.ResourceFiles,
	}
	_ = writeState(cfg, Status{CheckedAt: time.Now().UTC(), CurrentVersion: version, LatestVersion: version})
	return result, nil
}

var runCandidateActivation = func(
	ctx context.Context,
	binary string,
	payload string,
	targetHome string,
	coordinatorPID int,
) (activation.UpgradeResult, error) {
	args := []string{
		"--json", "server", "upgrade-activate",
		"--payload", payload,
		"--target-home", targetHome,
	}
	if coordinatorPID > 0 {
		args = append(
			args, "--coordinator-pid", strconv.Itoa(coordinatorPID),
		)
	}
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = replaceEnv(os.Environ(), "SN_CLI_HOME", targetHome)
	output, err := command.CombinedOutput()
	if err != nil {
		return activation.UpgradeResult{}, fmt.Errorf(
			"activate candidate: %w: %s",
			err, strings.TrimSpace(string(output)),
		)
	}
	if len(output) > 1<<20 {
		return activation.UpgradeResult{}, fmt.Errorf(
			"candidate activation output exceeds 1048576 bytes",
		)
	}
	var response struct {
		SchemaVersion   int                      `json:"schema_version"`
		ContractVersion int                      `json:"contract_version"`
		Activated       bool                     `json:"activated"`
		Activation      activation.UpgradeResult `json:"activation"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(output)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return activation.UpgradeResult{}, fmt.Errorf(
			"decode candidate activation result: %w", err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return activation.UpgradeResult{}, fmt.Errorf(
			"candidate activation result has trailing JSON",
		)
	}
	if response.SchemaVersion != 1 || response.ContractVersion != 3 ||
		!response.Activated ||
		filepath.Clean(response.Activation.TargetHome) !=
			filepath.Clean(targetHome) {
		return activation.UpgradeResult{}, fmt.Errorf(
			"candidate activation result does not match target home",
		)
	}
	return response.Activation, nil
}

func latestRelease(ctx context.Context, cfg *config.Config) (string, error) {
	if version := strings.TrimSpace(os.Getenv("SN_CLI_LATEST_VERSION")); version != "" {
		return version, nil
	}
	endpoint := strings.TrimSpace(os.Getenv("SN_CLI_RELEASE_API_URL"))
	if endpoint == "" {
		endpoint = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", cfg.Update.Repository)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "sn-cli-update")
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return "", fmt.Errorf("check latest release: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("check latest release: HTTP %d", response.StatusCode)
	}
	var payload struct {
		TagName string `json:"tag_name"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("decode latest release: %w", err)
	}
	if strings.TrimSpace(payload.TagName) == "" {
		return "", fmt.Errorf("latest release has no tag_name")
	}
	return payload.TagName, nil
}

func download(ctx context.Context, client *http.Client, source, target string) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", "sn-cli-update")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download %s: %w", source, err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("download %s: HTTP %d", source, response.StatusCode)
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, io.LimitReader(response.Body, 1<<30))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func validatePayload(
	binary, serverBinary, profiles, runtimeConfig, resources string,
) error {
	for name, path := range map[string]string{
		"sn-cli": binary, "sn-server": serverBinary,
	} {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 {
			return fmt.Errorf("release archive has no executable %s", name)
		}
	}
	if info, err := os.Stat(profiles); err != nil || !info.IsDir() {
		return fmt.Errorf("release archive has no configs directory")
	}
	if info, err := os.Stat(runtimeConfig); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("release archive has no runtime.json")
	}
	if info, err := os.Stat(resources); err != nil || !info.IsDir() {
		return fmt.Errorf("release archive has no resources directory")
	}
	return nil
}

func copyMissingFile(source, target string) (bool, error) {
	sourceInfo, err := os.Lstat(source)
	if err != nil {
		return false, err
	}
	if sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
		return false, fmt.Errorf("source is not a regular file: %s", source)
	}
	if targetInfo, err := os.Lstat(target); err == nil {
		if targetInfo.Mode()&os.ModeSymlink != 0 || !targetInfo.Mode().IsRegular() {
			return false, fmt.Errorf("target is not a regular file: %s", target)
		}
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return false, err
	}
	input, err := os.Open(source)
	if err != nil {
		return false, err
	}
	defer input.Close()
	output, err := os.OpenFile(
		target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, sourceInfo.Mode().Perm(),
	)
	if err != nil {
		return false, err
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(target)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return false, err
	}
	if err := output.Close(); err != nil {
		return false, err
	}
	ok = true
	return true, nil
}

func validateBinary(ctx context.Context, binary, home string) error {
	for _, args := range [][]string{{"profile", "check"}, {"server", "info"}} {
		command := exec.CommandContext(ctx, binary, args...)
		command.Env = replaceEnv(os.Environ(), "SN_CLI_HOME", home)
		output, err := command.CombinedOutput()
		if err != nil {
			return fmt.Errorf("validate new sn-cli %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func installBinary(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporary := fmt.Sprintf("%s.new.%d", target, os.Getpid())
	output, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	}
	if err := os.Rename(temporary, target); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace sn-cli binary: %w", err)
	}
	return nil
}

func replaceEnv(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func platform() (string, string, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return "", "", fmt.Errorf("unsupported operating system: %s", runtime.GOOS)
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return "", "", fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
	return runtime.GOOS, runtime.GOARCH, nil
}

func normalizeVersion(value string) string {
	return strings.TrimPrefix(strings.TrimSpace(value), "v")
}

func writeState(cfg *config.Config, status Status) error {
	path := cfg.UpdateStateFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state{CheckedAt: status.CheckedAt, CurrentVersion: status.CurrentVersion, LatestVersion: status.LatestVersion}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
