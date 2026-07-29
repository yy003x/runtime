package session

import (
	"fmt"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
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
	defer os.Remove(path)
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() ||
		info.Mode().Perm() != 0o600 ||
		info.Size() > maxInvocationManifestBytes {
		return fmt.Errorf("invalid invocation manifest")
	}
	data, err := io.ReadAll(io.LimitReader(
		file, maxInvocationManifestBytes+1,
	))
	if err != nil {
		return err
	}
	if len(data) > maxInvocationManifestBytes {
		return fmt.Errorf("invocation manifest exceeds size limit")
	}
	var manifest helperInvocationManifest
	if err := decodeStrict(data, &manifest); err != nil {
		return err
	}
	if manifest.Path == "" || len(manifest.Argv) == 0 ||
		manifest.CWD == "" {
		return fmt.Errorf("invocation manifest is incomplete")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	if err := os.Chdir(manifest.CWD); err != nil {
		return err
	}
	return syscall.Exec(
		manifest.Path, manifest.Argv, manifest.Environment,
	)
}
