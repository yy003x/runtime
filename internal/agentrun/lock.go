package agentrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type advisoryLock struct {
	file *os.File
}

func acquireAdvisoryLock(ctx context.Context, path string, blocking bool) (*advisoryLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	for {
		how := syscall.LOCK_EX
		if !blocking || ctx != nil {
			how |= syscall.LOCK_NB
		}
		err = syscall.Flock(int(file.Fd()), how)
		if err == nil {
			return &advisoryLock{file: file}, nil
		}
		if !blocking {
			_ = file.Close()
			return nil, err
		}
		if ctx != nil {
			select {
			case <-ctx.Done():
				_ = file.Close()
				return nil, ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
			continue
		}
		_ = file.Close()
		return nil, err
	}
}

func (l *advisoryLock) release() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}

func (s *Service) acquireRunLock(ctx context.Context, runID string) (*advisoryLock, error) {
	path := filepath.Join(s.StateDir, "runs", "locks", runID+".lock")
	lock, err := acquireAdvisoryLock(ctx, path, true)
	if err != nil {
		return nil, fmt.Errorf("acquire run lock: %w", err)
	}
	return lock, nil
}

func (s *Service) acquireConcurrencySlot() (*advisoryLock, error) {
	for slot := 0; slot < s.MaxConcurrency; slot++ {
		path := filepath.Join(s.StateDir, "runs", "concurrency", fmt.Sprintf("slot-%d.lock", slot))
		lock, err := acquireAdvisoryLock(context.Background(), path, false)
		if err == nil {
			return lock, nil
		}
	}
	return nil, fmt.Errorf("max_concurrency=%d reached", s.MaxConcurrency)
}

func withAdvisoryFileLock(path string, operation func() error) error {
	lock, err := acquireAdvisoryLock(context.Background(), path, true)
	if err != nil {
		return err
	}
	defer lock.release()
	return operation()
}
