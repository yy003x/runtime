// Package codexauth provides the fixed Codex OAuth transport and private local
// credential storage. It never reads another application's credential cache.
package codexauth

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
)

const Provider = "openai-codex"

type Credential struct {
	Type         string    `json:"type"`
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	AccountID    string    `json:"account_id"`
}

func (credential Credential) Validate() error {
	if credential.Type != "oauth" || !validToken(credential.AccessToken) ||
		!validToken(credential.RefreshToken) || !validToken(credential.AccountID) || credential.ExpiresAt.IsZero() {
		return fmt.Errorf("invalid OpenAI Codex credential; log in again")
	}
	return nil
}

func validToken(value string) bool {
	return value != "" && len(value) <= 32<<10 && !strings.ContainsAny(value, "\r\n\x00\t ")
}

type Document struct {
	SchemaVersion int                   `json:"schema_version"`
	Revision      uint64                `json:"revision"`
	Providers     map[string]Credential `json:"providers"`
}

func emptyDocument() Document {
	return Document{SchemaVersion: 1, Providers: make(map[string]Credential)}
}

func (document Document) validate() error {
	if document.SchemaVersion != 1 || document.Providers == nil {
		return fmt.Errorf("invalid auth.json schema")
	}
	for name, credential := range document.Providers {
		if name != Provider {
			return fmt.Errorf("unsupported provider in auth.json")
		}
		if err := credential.Validate(); err != nil {
			return err
		}
	}
	return nil
}

type Store struct {
	StateDir string
}

// Read is read-only, including when no credentials have ever been stored.
// Atomic replacement makes the snapshot coherent without creating a lock file.
func (store Store) Read() (Document, error) {
	directory, err := store.openDirectory(false)
	if errors.Is(err, os.ErrNotExist) {
		return emptyDocument(), nil
	}
	if err != nil {
		return Document{}, err
	}
	defer directory.Close()
	return readDocument(int(directory.Fd()))
}

// Update serializes login, logout and refresh across processes. The callback
// may refresh a token while holding the lock, so concurrent refreshes re-read
// the newly rotated token. The stable lock file is never unlinked.
func (store Store) Update(ctx context.Context, update func(*Document) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	directory, err := store.openDirectory(true)
	if err != nil {
		return err
	}
	defer directory.Close()
	dirFD := int(directory.Fd())
	lock, err := openPrivateFile(dirFD, "auth.lock", unix.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		lock, err = openPrivateFile(dirFD, "auth.lock", unix.O_CREAT|unix.O_EXCL|unix.O_RDWR)
		if errors.Is(err, os.ErrExist) {
			lock, err = openPrivateFile(dirFD, "auth.lock", unix.O_RDWR)
		}
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	for {
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN && err != unix.EINTR {
			return fmt.Errorf("cannot lock auth.json")
		}
		if err := wait(ctx, 25*time.Millisecond); err != nil {
			return err
		}
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck
	if err := ctx.Err(); err != nil {
		return err
	}
	document, err := readDocument(dirFD)
	if err != nil {
		return err
	}
	changed, err := update(&document)
	if err != nil || !changed {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if document.Revision == ^uint64(0) {
		return fmt.Errorf("auth.json revision overflow")
	}
	document.Revision++
	if err := document.validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return fmt.Errorf("cannot encode auth.json")
	}
	name := ".auth-" + rand.Text() + ".tmp"
	file, err := openPrivateFile(dirFD, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY)
	if err != nil {
		return err
	}
	defer file.Close()
	defer unix.Unlinkat(dirFD, name, 0) //nolint:errcheck
	if _, err := file.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("cannot write auth.json")
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("cannot sync auth.json")
	}
	if err := unix.Renameat(dirFD, name, dirFD, "auth.json"); err != nil {
		return fmt.Errorf("cannot replace auth.json")
	}
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("cannot sync credential directory")
	}
	return nil
}

func (store Store) openDirectory(create bool) (*os.File, error) {
	if create {
		if err := os.MkdirAll(store.StateDir, 0o700); err != nil {
			return nil, fmt.Errorf("cannot create credential directory: %w", err)
		}
	}
	fd, err := unix.Open(store.StateDir, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("cannot open credential directory: %w", err)
	}
	file := os.NewFile(uintptr(fd), store.StateDir)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		file.Close()
		return nil, fmt.Errorf("credential directory must be owned by the current user with mode 0700")
	}
	return file, nil
}

func openPrivateFile(dirFD int, name string, flags int) (*os.File, error) {
	fd, err := unix.Openat(dirFD, name, flags|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0o600)
	if err != nil {
		return nil, fmt.Errorf("cannot open private credential file %s: %w", name, err)
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || stat.Mode&0o077 != 0 {
		file.Close()
		return nil, fmt.Errorf("credential files must be regular, single-link, owned by the current user, and mode 0600")
	}
	return file, nil
}

func readDocument(dirFD int) (Document, error) {
	file, err := openPrivateFile(dirFD, "auth.json", unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return emptyDocument(), nil
	}
	if err != nil {
		return Document{}, err
	}
	defer file.Close()
	var stored struct {
		SchemaVersion int                   `json:"schema_version"`
		Revision      *uint64               `json:"revision"`
		Providers     map[string]Credential `json:"providers"`
	}
	if err := strictjson.DecodeObjectNoNulls(file, 128<<10, &stored); err != nil {
		// Decoder errors can echo arbitrary secret values or object names.
		return Document{}, fmt.Errorf("invalid auth.json document; credential contents are hidden")
	}
	if stored.Revision == nil {
		return Document{}, fmt.Errorf("auth.json revision is required")
	}
	document := Document{SchemaVersion: stored.SchemaVersion, Revision: *stored.Revision, Providers: stored.Providers}
	return document, document.validate()
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
