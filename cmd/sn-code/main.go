// sn-code 是 SN Runtime 的交互式编码代理 TUI：复用 active home 的 API
// Profile 与模型驱动，会话落在 {home}/sncode/sessions。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/sn-code/internal/application/codingagent"
	"github.com/yy003x/sn-code/internal/application/codingbootstrap"
	"github.com/yy003x/sn-code/internal/infrastructure/buildinfo"
	"github.com/yy003x/sn-code/internal/infrastructure/codingsessions"
	"github.com/yy003x/sn-code/internal/infrastructure/layout"
	"github.com/yy003x/sn-code/internal/interfaces/codingacp"
	"github.com/yy003x/sn-code/internal/interfaces/codingtui"
)

// main 执行 main 对应的包内处理，并遵循调用方提供的输入契约。
func main() {
	// 首参 `acp` 进入 Agent Client Protocol 的 stdio 模式；其余参数继续
	// 走交互 TUI。ACP 模式不要求 TTY，stdin/stdout 承载 JSON-RPC。
	args := os.Args[1:]
	acpMode := len(args) > 0 && args[0] == "acp"
	if acpMode {
		args = args[1:]
	}
	profileFlag := flag.String("profile", "", "API profile id to use (default: last used or first available)")
	resumeFlag := flag.String("resume", "", "resume a session: session id prefix, or \"list\" to pick")
	showVersion := flag.Bool("version", false, "print version and exit")
	_ = flag.CommandLine.Parse(args)

	if *showVersion {
		fmt.Println(buildinfo.String("sn-code"))
		return
	}
	var err error
	if acpMode {
		err = runACP(*profileFlag)
	} else {
		err = run(*profileFlag, *resumeFlag)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "sn-code:", err)
		os.Exit(1)
	}
}

// run 启动交互 TUI；profileFlag 指定可选 API Profile，resumeFlag 指定
// 待恢复会话 ID 前缀或 list 选择器。返回 error 报告终端、配置、会话加载
// 或 TUI 运行失败；启动过程会加载 active home 服务并写入会话文件。
func run(profileFlag, resumeFlag string) error {
	if !isTerminal(os.Stdin) || !isTerminal(os.Stdout) {
		return errors.New("sn-code needs an interactive terminal (stdin and stdout must be TTYs)")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	paths, err := layout.Resolve()
	if err != nil {
		return err
	}
	services, err := codingbootstrap.Load(paths)
	if err != nil {
		return err
	}
	if len(services.Profiles.IDs()) == 0 {
		return errors.New("no API profiles configured; add one under $SN_CLI_HOME/configs and retry")
	}
	factory := &snCodeFactory{services: services, cwd: cwd, overrideProfile: profileFlag}
	app, err := codingtui.New(codingtui.Deps{
		Factory:  factory,
		Profiles: services.Profiles,
		Resume: func() ([]codingtui.SessionSummary, error) {
			return listSummaries(services, cwd)
		},
		CWD: cwd,
	})
	if err != nil {
		return err
	}

	resumePath := ""
	switch {
	case resumeFlag == "":
	case resumeFlag == "list":
		summaries, err := listSummaries(services, cwd)
		if err != nil {
			return err
		}
		if len(summaries) == 0 {
			return errors.New("no previous sessions in this directory")
		}
		// list 模式启动后由 TUI 打开选择器：先加载最新会话占位，再弹选择。
		resumePath = summaries[0].Path
		app.OpenResumeAtStart()
	default:
		matched, err := matchSession(services, cwd, resumeFlag)
		if err != nil {
			return err
		}
		resumePath = matched
	}
	if err := app.StartSession(resumePath); err != nil {
		return err
	}
	return app.Run()
}

// runACP 以 Agent Client Protocol v1 的 stdio 模式服务编辑器等 ACP 客户端：
// 复用 active home 的服务与会话内核，stdout 只输出协议 JSON，不进入 TUI。
// 参数 profileFlag 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func runACP(profileFlag string) error {
	paths, err := layout.Resolve()
	if err != nil {
		return err
	}
	services, err := codingbootstrap.Load(paths)
	if err != nil {
		return err
	}
	if len(services.Profiles.IDs()) == 0 {
		return errors.New("no API profiles configured; add one under $SN_CLI_HOME/configs and retry")
	}
	factory := &snCodeFactory{services: services, overrideProfile: profileFlag}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	server := codingacp.NewServer(os.Stdout, codingacp.Options{
		Version: buildinfo.Summary(),
		Factory: factory,
	})
	return server.Serve(ctx, os.Stdin)
}

// listSummaries 执行 listSummaries 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 services、cwd 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []codingtui.SessionSummary、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func listSummaries(services *codingbootstrap.Services, cwd string) ([]codingtui.SessionSummary, error) {
	summaries, err := services.Sessions.List(cwd)
	if err != nil {
		return nil, err
	}
	values := make([]codingtui.SessionSummary, 0, len(summaries))
	for _, summary := range summaries {
		values = append(values, codingtui.SessionSummary{
			ID:        summary.ID,
			Name:      summary.Name,
			Path:      summary.Path,
			Timestamp: summary.Timestamp,
			Entries:   summary.Entries,
		})
	}
	return values, nil
}

// matchSession 执行 matchSession 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 services、cwd、prefix 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func matchSession(services *codingbootstrap.Services, cwd, prefix string) (string, error) {
	summaries, err := listSummaries(services, cwd)
	if err != nil {
		return "", err
	}
	var matches []codingtui.SessionSummary
	for _, summary := range summaries {
		if strings.HasPrefix(summary.ID, prefix) ||
			(summary.Name != "" && strings.HasPrefix(summary.Name, prefix)) {
			matches = append(matches, summary)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0].Path, nil
	case 0:
		return "", fmt.Errorf("no session matches %q in this directory", prefix)
	default:
		return "", fmt.Errorf("%q matches %d sessions; use a longer prefix", prefix, len(matches))
	}
}

// snCodeFactory 把编程产品服务适配为 TUI 与 ACP 的会话工厂。
type snCodeFactory struct {
	services        *codingbootstrap.Services
	cwd             string
	overrideProfile string
}

// ACP 模式按会话 cwd 建立内核，不经 TUI 的固定 cwd。
var _ codingacp.SessionFactory = (*snCodeFactory)(nil)

// NewSession 为 ACP 连接在指定 cwd 下创建新会话。
// 参数 cwd、gate、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (factory *snCodeFactory) NewSession(
	cwd string,
	gate codingagent.ToolGate,
	emit codingagent.EventSink,
) (*codingagent.AgentSession, error) {
	store, err := factory.services.Sessions.Create(time.Now(), cwd, "")
	if err != nil {
		return nil, err
	}
	return factory.build(store, gate, emit, cwd)
}

// LoadSession 在指定 cwd 下按完整会话 ID 恢复既有会话；ACP 的 sessionId
// 是精确 ID，不使用前缀匹配。
// 参数 cwd、sessionID、gate、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (factory *snCodeFactory) LoadSession(
	cwd, sessionID string,
	gate codingagent.ToolGate,
	emit codingagent.EventSink,
) (*codingagent.AgentSession, error) {
	summaries, err := factory.services.Sessions.List(cwd)
	if err != nil {
		return nil, err
	}
	for _, summary := range summaries {
		if summary.ID != sessionID {
			continue
		}
		store, err := factory.services.Sessions.OpenStoreFile(summary.Path)
		if err != nil {
			return nil, err
		}
		return factory.build(store, gate, emit, cwd)
	}
	return nil, fmt.Errorf("session %q was not found in %s", sessionID, cwd)
}

// Create 执行 Create 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 gate、emit、parentSession 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (factory *snCodeFactory) Create(
	gate codingagent.ToolGate,
	emit codingagent.EventSink,
	parentSession string,
) (*codingagent.AgentSession, error) {
	store, err := factory.services.Sessions.Create(time.Now(), factory.cwd, parentSession)
	if err != nil {
		return nil, err
	}
	return factory.build(store, gate, emit, factory.cwd)
}

// Open 执行 Open 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 gate、emit、path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (factory *snCodeFactory) Open(
	gate codingagent.ToolGate,
	emit codingagent.EventSink,
	path string,
) (*codingagent.AgentSession, error) {
	store, err := factory.services.Sessions.OpenStoreFile(path)
	if err != nil {
		return nil, err
	}
	return factory.build(store, gate, emit, factory.cwd)
}

// build 执行 build 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 store、gate、emit、cwd 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (factory *snCodeFactory) build(
	store *codingsessions.Session,
	gate codingagent.ToolGate,
	emit codingagent.EventSink,
	cwd string,
) (*codingagent.AgentSession, error) {
	return codingagent.New(codingagent.Deps{
		Model:    factory.services.Model,
		Registry: factory.services.Registry,
		Policy:   factory.services.Policy,
		Profiles: factory.services.Profiles,
		Session:  store,
		Gate:     gate,
		Emit:     emit,
		CWD:      cwd,
		AgentDir: factory.services.AgentDir,
	}, factory.DefaultProfile())
}

// DefaultProfile 执行 DefaultProfile 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (factory *snCodeFactory) DefaultProfile() string {
	if factory.overrideProfile != "" {
		return factory.overrideProfile
	}
	preferencesPath := filepath.Join(factory.services.AgentDir, "preferences.json")
	preferences, err := codingsessions.LoadPreferences(preferencesPath)
	if err == nil && preferences.LastProfile != "" {
		if _, ok := factory.services.Profiles.Get(preferences.LastProfile); ok {
			return preferences.LastProfile
		}
	}
	ids := factory.services.Profiles.IDs()
	if len(ids) > 0 {
		return ids[0]
	}
	return ""
}

// SaveDefaultProfile 执行 SaveDefaultProfile 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 id 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (factory *snCodeFactory) SaveDefaultProfile(id string) error {
	preferencesPath := filepath.Join(factory.services.AgentDir, "preferences.json")
	return codingsessions.SavePreferences(preferencesPath, codingsessions.Preferences{
		LastProfile: id,
	})
}

// isTerminal 执行 isTerminal 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 file 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func isTerminal(file *os.File) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
