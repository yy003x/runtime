package session

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
)

func init() {
	if len(os.Args) != 2 || os.Args[1] != execHelperArgument {
		return
	}
	if err := runPrivateExecHelper(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "private Session exec helper failed")
		os.Exit(125)
	}
	os.Exit(0)
}

func runPrivateExecHelper() error {
	handshake := os.NewFile(uintptr(3), "session-exec-handshake")
	if handshake == nil {
		return fmt.Errorf("handshake is unavailable")
	}
	defer handshake.Close()
	var signal [1]byte
	if _, err := io.ReadFull(handshake, signal[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return nil
		}
		return err
	}
	if signal[0] != 1 {
		return fmt.Errorf("invalid handshake")
	}
	if err := handshake.Close(); err != nil {
		return err
	}
	path := os.Getenv(execHelperManifestEnv)
	if path == "" {
		return fmt.Errorf("manifest is required")
	}
	directoryIdentity, err := decodeSafeFileIdentity(
		os.Getenv(execHelperManifestDirIDEnv),
	)
	if err != nil {
		return err
	}
	fileIdentity, err := decodeSafeFileIdentity(
		os.Getenv(execHelperManifestFileIDEnv),
	)
	if err != nil {
		return err
	}
	manifest, err := consumeInvocationManifest(
		path, directoryIdentity, fileIdentity,
	)
	if err != nil {
		return err
	}
	if err := os.Chdir(manifest.CWD); err != nil {
		return err
	}
	return syscall.Exec(
		manifest.Path, manifest.Argv, manifest.Environment,
	)
}

func consumeInvocationManifest(
	path string,
	expectedDirectory safeFileIdentity,
	expectedFile safeFileIdentity,
) (helperInvocationManifest, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return helperInvocationManifest{}, fmt.Errorf(
			"invocation manifest path must be absolute and clean",
		)
	}
	name := filepath.Base(path)
	if err := safeName(name); err != nil {
		return helperInvocationManifest{}, err
	}
	directory, err := openSafeDirectory(filepath.Dir(path))
	if err != nil {
		return helperInvocationManifest{}, err
	}
	defer directory.close()
	identity, err := directory.identity()
	if err != nil {
		return helperInvocationManifest{}, err
	}
	if identity != expectedDirectory {
		return helperInvocationManifest{}, fmt.Errorf(
			"private invocation directory changed identity",
		)
	}
	data, entry, err := directory.readRegularFact(
		name, maxInvocationManifestBytes,
	)
	if err != nil {
		return helperInvocationManifest{}, err
	}
	if entry.identity() != expectedFile ||
		!entry.isRegular() ||
		entry.nlink != 1 ||
		os.FileMode(entry.mode).Perm() != 0o600 {
		return helperInvocationManifest{}, fmt.Errorf(
			"invalid invocation manifest identity or mode",
		)
	}
	var manifest helperInvocationManifest
	if err := decodeStrict(data, &manifest); err != nil {
		return helperInvocationManifest{}, err
	}
	if manifest.Path == "" ||
		len(manifest.Argv) == 0 ||
		manifest.CWD == "" {
		return helperInvocationManifest{}, fmt.Errorf(
			"invocation manifest is incomplete",
		)
	}
	if err := directory.removeRegularRequired(
		name, &expectedFile,
	); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return helperInvocationManifest{}, fmt.Errorf(
				"invocation manifest disappeared before consumption",
			)
		}
		return helperInvocationManifest{}, err
	}
	return manifest, nil
}
