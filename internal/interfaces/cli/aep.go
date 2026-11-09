package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/yy003x/sn-code/internal/application/runtimebootstrap"
	"github.com/yy003x/sn-code/internal/infrastructure/buildinfo"
	"github.com/yy003x/sn-code/internal/infrastructure/layout"
	aeptransport "github.com/yy003x/sn-code/pkg/transport/aep"
)

// runAEPNamespace 启动 AEP stdio 服务端：stdin/stdout 只承载 NDJSON
// JSON-RPC 协议消息，诊断写 stderr。进程自带一个 scheduler worker 执行
// queued session Run（与 sn-server 同一 SQLite 队列语义）。
// 参数 paths、args、output 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func runAEPNamespace(paths layout.Paths, args []string, output *cliOutput) error {
	if len(args) != 0 {
		return cliValidationf("aep does not accept arguments")
	}
	if output.JSON() {
		return cliValidationf("aep does not support --json; stdout is the AEP protocol stream")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	// 加入共享队列不代表旧 owner 已退出；全库恢复只由独占恢复入口执行。
	services, err := runtimebootstrap.LoadServices(
		paths, cwd, fixedNamespaces...,
	)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
	server, err := aeptransport.NewServer(aeptransport.ServerConfig{
		Profiles: services.Profiles, Runs: services.Runs,
		Server: aeptransport.ServerInfo{
			Name: "sn-runtime", Version: buildinfo.String("sn-cli"),
		},
		InvocationBase: cwd,
		Diagnostics: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	})
	if err != nil {
		return err
	}
	runContext, stop := signal.NotifyContext(
		context.Background(), os.Interrupt, syscall.SIGTERM,
	)
	defer stop()
	// 首个信号触发取消后立即注销信号接管：graceful settle 卡滞时二次
	// SIGINT/SIGTERM 恢复默认终止语义，允许用户强制退出。
	stopSignalsAfterCancel := context.AfterFunc(runContext, stop)
	defer stopSignalsAfterCancel()
	serveContext, stopServing := context.WithCancel(runContext)
	defer stopServing()
	// worker 不挂在信号 ctx 上：Serve 关停期间的取消收口（reservation +
	// TERM→KILL → settle）依赖 worker 继续执行，Run 终态确认后才停 worker。
	workerContext, stopWorker := context.WithCancel(context.Background())
	defer stopWorker()
	workerID := fmt.Sprintf("sn-cli-aep-%d", os.Getpid())
	workerDone := make(chan error, 1)
	go func() {
		err := services.Runs.Worker(
			workerContext, workerID, services.Config.PollInterval(),
		)
		if workerContext.Err() != nil {
			err = nil
		} else {
			if err == nil {
				err = fmt.Errorf("aep worker exited unexpectedly")
			}
			stopServing()
		}
		workerDone <- err
	}()
	serveErr := server.Serve(serveContext, os.Stdin, os.Stdout)
	// Serve 返回即 Run 已 graceful 收口；停掉 worker 再关闭 Run store。
	stopWorker()
	workerErr := <-workerDone
	return errors.Join(workerErr, serveErr)
}
