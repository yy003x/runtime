package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/infrastructure/activationgate"
	"github.com/yy003x/runtime/pkg/contract"
)

const (
	targetReadyPathEnv   = "SN_TMUX_TARGET_READY_PATH"
	targetReadyNonceEnv  = "SN_TMUX_TARGET_READY_NONCE"
	targetReadyDigestEnv = "SN_TMUX_TARGET_READY_DIGEST"
)

// HelperCommandName is the hidden argv token the CLI root must dispatch before
// loading Runtime configuration.
const HelperCommandName = helperCommandName

// RunHelper executes the private pane bootstrap helper. It never reads Profile
// or Session state and only accepts a no-follow manifest created by Service.
func RunHelper(args []string) error {
	if len(args) != 2 || args[0] != "--manifest" ||
		!filepath.IsAbs(args[1]) {
		return fmt.Errorf("usage: %s --manifest <absolute-path>", helperCommandName)
	}
	manifestPath := filepath.Clean(args[1])
	manifestDir := filepath.Dir(manifestPath)
	if err := requirePrivateDir(manifestDir, os.Getuid()); err != nil {
		return fmt.Errorf("validate Tmux helper directory: %w", err)
	}
	var manifest launchManifest
	if err := decodePrivateJSON(
		manifestPath, maxManifestBytes, os.Getuid(), &manifest,
	); err != nil {
		return fmt.Errorf("read Tmux launch manifest: %w", err)
	}
	if manifest.SchemaVersion != WindowSchemaVersion ||
		manifest.OwnerUID != os.Getuid() ||
		manifest.Home == "" || manifest.Nonce == "" ||
		manifest.Path == "" || len(manifest.Argv) == 0 ||
		manifest.CWD == "" || manifest.ExecutableIdentity == "" ||
		manifest.GateTimeoutMS <= 0 {
		return fmt.Errorf("Tmux launch manifest is incomplete")
	}
	home, err := canonicalHome(manifest.Home)
	if err != nil || home != manifest.Home ||
		manifestDir != filepath.Join(home, "tmp", "tmux") {
		return fmt.Errorf("Tmux helper manifest is outside Runtime home")
	}
	if err := activationgate.RequireOpen(
		filepath.Join(home, "state"),
	); err != nil {
		return fmt.Errorf("Tmux helper activation gate: %w", err)
	}
	if !incarnationPattern.MatchString(manifest.Nonce) {
		return fmt.Errorf("Tmux launch manifest nonce is invalid")
	}
	base := filepath.Join(manifestDir, "launch-"+manifest.Nonce)
	if manifestPath != base+".json" ||
		manifest.ReadyPath != base+".ready" ||
		manifest.GoPath != base+".go" ||
		manifest.StatusPath != base+".status" {
		return fmt.Errorf("Tmux helper paths do not match launch identity")
	}
	if err := validateManifest(manifest); err != nil {
		return err
	}
	identity, err := lookupProcessIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("identify Tmux helper: %w", err)
	}
	pgid, err := syscall.Getpgid(os.Getpid())
	if err != nil {
		return fmt.Errorf("identify Tmux helper process group: %w", err)
	}
	ready := readyFact{
		SchemaVersion: WindowSchemaVersion, Nonce: manifest.Nonce,
		PID: os.Getpid(), PGID: pgid, ProcessStart: identity.StartToken,
		Executable:         identity.Executable,
		ExecutableIdentity: identity.ExecutableIdentity,
		ManifestDigest:     manifest.ManifestDigest,
	}
	if err := writeJSONPrivate(manifest.ReadyPath, ready, os.Getuid()); err != nil {
		return fmt.Errorf("write Tmux helper ready fact: %w", err)
	}
	goValue, err := waitForGo(manifest)
	if err != nil {
		writeHelperFailure(manifest, "wait for Tmux launch gate")
		return err
	}
	if goValue.Nonce != manifest.Nonce ||
		goValue.ManifestDigest != manifest.ManifestDigest {
		writeHelperFailure(manifest, "Tmux launch gate identity mismatch")
		return fmt.Errorf("Tmux launch gate identity mismatch")
	}
	var current launchManifest
	if err := decodePrivateJSON(
		manifestPath, maxManifestBytes, os.Getuid(), &current,
	); err != nil {
		writeHelperFailure(manifest, "reopen Tmux launch manifest")
		return err
	}
	if err := validateManifest(current); err != nil ||
		current.ManifestDigest != manifest.ManifestDigest {
		writeHelperFailure(manifest, "Tmux launch manifest changed")
		if err != nil {
			return err
		}
		return fmt.Errorf("Tmux launch manifest changed")
	}
	if err := activationgate.RequireOpen(
		filepath.Join(home, "state"),
	); err != nil {
		writeHelperFailure(manifest, "Runtime activation gate is active")
		return fmt.Errorf("Tmux helper activation gate: %w", err)
	}
	if err := consumeLaunchFiles(manifestPath, manifest); err != nil {
		writeHelperFailure(manifest, "consume Tmux launch manifest")
		return err
	}
	if err := os.Chdir(current.CWD); err != nil {
		writeHelperFailure(manifest, "enter target working directory")
		return err
	}
	path, identityValue, err := executableIdentity(current.Path)
	if err != nil || identityValue != current.ExecutableIdentity {
		writeHelperFailure(manifest, "target executable identity changed")
		if err != nil {
			return err
		}
		return fmt.Errorf("target executable identity changed")
	}
	environment := exactTargetEnvironment(current.Environment, os.Environ())
	if current.TargetReadyPath != "" {
		environment = append(
			environment,
			targetReadyPathEnv+"="+current.TargetReadyPath,
			targetReadyNonceEnv+"="+current.Nonce,
			targetReadyDigestEnv+"="+current.ManifestDigest,
		)
	}
	if err := validateExactEnvironment(environment); err != nil {
		writeHelperFailure(manifest, "target environment is invalid")
		return err
	}
	if err := syscall.Exec(path, current.Argv, environment); err != nil {
		writeHelperFailure(manifest, "exec target command")
		return fmt.Errorf("exec target command: %w", err)
	}
	return nil
}

func validateManifest(value launchManifest) error {
	if value.SchemaVersion != WindowSchemaVersion ||
		value.OwnerUID != os.Getuid() ||
		value.ManifestDigest == "" {
		return fmt.Errorf("Tmux launch manifest identity is invalid")
	}
	expected, _, err := marshalManifest(value)
	if err != nil {
		return err
	}
	if expected != value.ManifestDigest {
		return fmt.Errorf("Tmux launch manifest digest mismatch")
	}
	if err := validateExactEnvironment(value.Environment); err != nil {
		return err
	}
	if len(value.Argv) == 0 || value.Argv[0] == "" {
		return fmt.Errorf("Tmux launch argv is required")
	}
	for _, argument := range value.Argv {
		if !utf8.ValidString(argument) || strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("Tmux launch argv is invalid")
		}
	}
	if !filepath.IsAbs(value.CWD) || !filepath.IsAbs(value.Path) {
		return fmt.Errorf("Tmux launch paths must be absolute")
	}
	if value.TargetReadyPath != "" {
		expected := filepath.Join(
			filepath.Dir(value.ReadyPath),
			"launch-"+value.Nonce+".target-ready",
		)
		if value.TargetReadyPath != expected {
			return fmt.Errorf("Tmux cooperative target ready path is invalid")
		}
	}
	if value.GateTimeoutMS <= 0 ||
		value.GateTimeoutMS > int64(time.Minute/time.Millisecond) {
		return fmt.Errorf("Tmux launch gate timeout is invalid")
	}
	return nil
}

func waitForGo(manifest launchManifest) (goFact, error) {
	if manifest.GateTimeoutMS <= 0 ||
		manifest.GateTimeoutMS > int64(time.Minute/time.Millisecond) {
		return goFact{}, fmt.Errorf("Tmux launch gate timeout is invalid")
	}
	timeout := time.Duration(manifest.GateTimeoutMS) * time.Millisecond
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var value goFact
		err := decodePrivateJSON(
			manifest.GoPath, 64<<10, os.Getuid(), &value,
		)
		if err == nil {
			return value, nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return goFact{}, tmuxTransportError(
		contract.ErrorTimeout, "Tmux launch gate timed out",
	)
}

func writeHelperFailure(manifest launchManifest, message string) {
	_ = writeJSONPrivate(
		manifest.StatusPath,
		helperStatus{
			SchemaVersion: WindowSchemaVersion, Nonce: manifest.Nonce,
			ManifestDigest: manifest.ManifestDigest,
			Error: &SafeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
				Message: message,
			},
		},
		os.Getuid(),
	)
	removeExact(
		filepath.Join(
			filepath.Dir(manifest.ReadyPath),
			"launch-"+manifest.Nonce+".json",
		),
		manifest.ReadyPath, manifest.GoPath,
	)
}

func consumeLaunchFiles(
	manifestPath string,
	manifest launchManifest,
) error {
	for _, path := range []string{
		manifestPath, manifest.ReadyPath, manifest.GoPath,
	} {
		info, err := os.Lstat(path)
		if err != nil {
			return fmt.Errorf("inspect consumed Tmux launch file: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() ||
			info.Mode().Perm() != 0o600 {
			return fmt.Errorf(
				"consumed Tmux launch file is not a private regular file",
			)
		}
		if err := requireOwner(info, os.Getuid(), path); err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("consume Tmux launch file: %w", err)
		}
	}
	return nil
}

func exactTargetEnvironment(
	configured []string,
	current []string,
) []string {
	reserved := map[string]string{}
	for _, value := range current {
		index := strings.IndexByte(value, '=')
		if index <= 0 {
			continue
		}
		switch value[:index] {
		case "TERM", "TMUX", "TMUX_PANE":
			reserved[value[:index]] = value[index+1:]
		}
	}
	result := make([]string, 0, len(configured)+3)
	for _, value := range configured {
		index := strings.IndexByte(value, '=')
		if index <= 0 {
			continue
		}
		switch value[:index] {
		case "TERM", "TMUX", "TMUX_PANE",
			targetReadyPathEnv, targetReadyNonceEnv, targetReadyDigestEnv:
			continue
		default:
			result = append(result, value)
		}
	}
	for _, name := range []string{"TERM", "TMUX", "TMUX_PANE"} {
		if value, exists := reserved[name]; exists {
			result = append(result, name+"="+value)
		}
	}
	sort.Strings(result)
	return result
}

// AcknowledgeTargetReady publishes the one-shot fact requested by a
// CooperativeReady invocation. It must be called by the target after exec and
// before it starts its own runtime initialization.
func AcknowledgeTargetReady() error {
	path := os.Getenv(targetReadyPathEnv)
	nonce := os.Getenv(targetReadyNonceEnv)
	digest := os.Getenv(targetReadyDigestEnv)
	if path == "" || nonce == "" || digest == "" {
		return fmt.Errorf("Tmux cooperative target ready environment is incomplete")
	}
	defer func() {
		_ = os.Unsetenv(targetReadyPathEnv)
		_ = os.Unsetenv(targetReadyNonceEnv)
		_ = os.Unsetenv(targetReadyDigestEnv)
	}()
	if !filepath.IsAbs(path) || !incarnationPattern.MatchString(nonce) ||
		filepath.Base(path) != "launch-"+nonce+".target-ready" {
		return fmt.Errorf("Tmux cooperative target ready identity is invalid")
	}
	if err := requirePrivateDir(filepath.Dir(path), os.Getuid()); err != nil {
		return fmt.Errorf("validate Tmux cooperative target ready directory: %w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		return fmt.Errorf("Tmux cooperative target ready fact already exists: mode=%s", info.Mode())
	} else if !os.IsNotExist(err) {
		return err
	}
	identity, err := lookupProcessIdentity(os.Getpid())
	if err != nil {
		return fmt.Errorf("identify Tmux cooperative target: %w", err)
	}
	fact := targetReadyFact{
		SchemaVersion: WindowSchemaVersion, Nonce: nonce,
		ManifestDigest: digest, PID: os.Getpid(),
		ProcessStart: identity.StartToken, Executable: identity.Executable,
		ExecutableIdentity: identity.ExecutableIdentity,
	}
	if err := writeJSONPrivate(path, fact, os.Getuid()); err != nil {
		return fmt.Errorf("write Tmux cooperative target ready fact: %w", err)
	}
	return nil
}
