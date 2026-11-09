// Package serverruntime 编排服务启停，不承担 CLI 参数或输出。
package serverruntime

import (
	"errors"
	"fmt"
	"time"

	"github.com/yy003x/sn-code/internal/application/activation"
	"github.com/yy003x/sn-code/internal/infrastructure/layout"
	"github.com/yy003x/sn-code/internal/infrastructure/serverprocess"
)

// Result 区分状态与本次操作是否实际改变状态。
type Result struct {
	Running bool
	PID     int
	Changed bool
}

func Start(paths layout.Paths) (result Result, err error) {
	err = serverprocess.WithLifecycleLock(paths, func() error {
		running, pid, err := serverprocess.Running(paths)
		if err != nil {
			return err
		}
		if running {
			result = Result{Running: true, PID: pid}
			return nil
		}
		pid, err = serverprocess.Launch(paths)
		if err != nil {
			return err
		}
		result = Result{Running: true, PID: pid, Changed: true}
		return nil
	})
	return
}

func Status(paths layout.Paths) (result Result, err error) {
	err = serverprocess.WithLifecycleLock(paths, func() error {
		var err error
		result.Running, result.PID, err = serverprocess.Running(paths)
		return err
	})
	return
}

func Stop(paths layout.Paths) (result Result, err error) {
	err = serverprocess.WithLifecycleLock(paths, func() error {
		var err error
		result, err = stopLocked(paths)
		return err
	})
	return
}

// StopForInstall 由 activation 在持有同一生命周期锁时调用，不能重复获取锁。
func StopForInstall(paths layout.Paths) error {
	_, err := stopLocked(paths)
	return err
}

// InspectForInstall 绑定当前进程身份，供 activation 校验安装前后的目标。
func InspectForInstall(paths layout.Paths) (activation.ManagedServerProcess, error) {
	running, pid, err := serverprocess.Running(paths)
	if err != nil {
		return activation.ManagedServerProcess{}, err
	}
	if !running {
		return activation.ManagedServerProcess{}, nil
	}
	record, err := serverprocess.ReadPID(paths.ServerPIDFile)
	if err != nil {
		return activation.ManagedServerProcess{}, err
	}
	if record.PID != pid {
		return activation.ManagedServerProcess{}, fmt.Errorf("sn-server process identity changed during install preflight")
	}
	return activation.ManagedServerProcess{PID: pid, StartToken: record.ProcessStart}, nil
}

func stopLocked(paths layout.Paths) (Result, error) {
	running, pid, err := serverprocess.Running(paths)
	if err != nil {
		return Result{}, err
	}
	if !running {
		serverprocess.RemovePID(paths)
		return Result{}, nil
	}
	if err := serverprocess.Terminate(pid); err != nil {
		return Result{}, err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		running, currentPID, err := serverprocess.Running(paths)
		if err == nil && (!running || currentPID != pid) {
			serverprocess.RemovePID(paths)
			return Result{PID: pid, Changed: true}, nil
		}
		if err != nil && !(errors.Is(err, serverprocess.ErrLeaseReleasedWhileAlive) && currentPID == pid) {
			return Result{}, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	return Result{}, fmt.Errorf("sn-server pid %d did not stop within 10s", pid)
}
