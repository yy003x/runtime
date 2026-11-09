//go:build darwin || linux

package codingtui

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/sn-code/internal/application/codingagent"
	"github.com/yy003x/sn-code/internal/infrastructure/terminalinput"
	"github.com/yy003x/sn-code/pkg/contract"
)

// SessionFactory 由 composition root 提供：创建与恢复 AgentSession。
type SessionFactory interface {
	// Create 执行 Create 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 gate、emit、parentSession 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	Create(gate codingagent.ToolGate, emit codingagent.EventSink, parentSession string) (*codingagent.AgentSession, error)
	// Open 执行 Open 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 gate、emit、path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	Open(gate codingagent.ToolGate, emit codingagent.EventSink, path string) (*codingagent.AgentSession, error)
	// DefaultProfile 执行 DefaultProfile 对应的包内处理，并遵循调用方提供的输入契约。
	// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	DefaultProfile() string
	// SaveDefaultProfile 执行 SaveDefaultProfile 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 id 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	SaveDefaultProfile(id string) error
}

// Deps 是构造 App 的依赖。
type Deps struct {
	Factory  SessionFactory
	Profiles codingagent.ProfileSource
	Resume   func() ([]SessionSummary, error) // 列出当前 cwd 的会话
	CWD      string
	Stdin    *os.File
	Stdout   io.Writer
}

// SessionSummary 是 /resume 选择器需要的会话摘要。
type SessionSummary struct {
	ID        string
	Name      string
	Path      string
	Timestamp time.Time
	Entries   int
}

// historyLimit 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
const historyLimit = 1000

// App 持有全部 UI 状态并驱动事件循环：键盘输入、agent 事件、确认门请求
// 与异步任务都汇入同一个 select，任何一路变更后重绘活动区。
type App struct {
	factory  SessionFactory
	profiles codingagent.ProfileSource
	resume   func() ([]SessionSummary, error)
	cwd      string

	term   *terminalinput.Terminal
	out    io.Writer
	paint  painter
	editor *Editor

	session *codingagent.AgentSession

	loopCtx    context.Context
	loopCancel context.CancelFunc
	inputCtx   context.Context
	inputStop  context.CancelFunc
	events     chan codingagent.Event
	gateReq    chan gateRequest
	uiTasks    chan func()
	runCancel  context.CancelFunc

	// 以下字段只在主循环 goroutine 访问。
	history     []string
	sessionName string
	streaming   strings.Builder
	busy        bool // 模型运行或手动压缩；shell 活动单独计数。
	shells      int
	status      string
	confirm     *gateRequest
	picker      *pickerState
	gitBranch   string
	quit        bool
	exitErr     error
	// resumeAtStart 由 --resume list 设置：启动后立即打开会话选择器。
	resumeAtStart bool
}

// OpenResumeAtStart 标记启动后打开 /resume 选择器。
func (app *App) OpenResumeAtStart() {
	app.resumeAtStart = true
}

// New 创建 App 并打开终端 raw mode。
// 参数 deps 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *App、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func New(deps Deps) (*App, error) {
	stdin := deps.Stdin
	if stdin == nil {
		stdin = os.Stdin
	}
	term, err := terminalinput.Open(stdin)
	if err != nil {
		return nil, fmt.Errorf("open terminal: %w", err)
	}
	out := deps.Stdout
	if out == nil {
		out = os.Stdout
	}
	loopCtx, loopCancel := context.WithCancel(context.Background())
	inputCtx, inputStop := context.WithCancel(context.Background())
	app := &App{
		factory:    deps.Factory,
		profiles:   deps.Profiles,
		resume:     deps.Resume,
		cwd:        deps.CWD,
		term:       term,
		out:        out,
		paint:      painter{out: out},
		editor:     NewEditor(),
		loopCtx:    loopCtx,
		loopCancel: loopCancel,
		inputCtx:   inputCtx,
		inputStop:  inputStop,
		events:     make(chan codingagent.Event, 256),
		gateReq:    make(chan gateRequest, 4),
		uiTasks:    make(chan func(), 16),
	}
	app.refreshBranch("")
	return app, nil
}

// Session 返回当前会话（测试与外层组装用）。
func (app *App) Session() *codingagent.AgentSession { return app.session }

// emit 是注入 AgentSession 的事件出口：delta 可丢帧，其余事件有界等待。
// 参数 event 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (app *App) emit(event codingagent.Event) error {
	select {
	case app.events <- event:
		return nil
	default:
	}
	if event.Type == codingagent.EventTextDelta {
		return nil
	}
	select {
	case app.events <- event:
		return nil
	case <-app.loopCtx.Done():
		return app.loopCtx.Err()
	}
}

// gate 执行 gate 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 codingagent.ToolGate 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (app *App) gate() codingagent.ToolGate {
	return appGate{requests: app.gateReq}
}

// StartSession 创建初始会话；resumePath 非空时恢复该会话文件。
// 参数 resumePath 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (app *App) StartSession(resumePath string) error {
	if resumePath != "" {
		session, err := app.factory.Open(app.gate(), app.emit, resumePath)
		if err != nil {
			return err
		}
		app.session = session
		app.sessionName = session.Name()
		return nil
	}
	session, err := app.factory.Create(app.gate(), app.emit, "")
	if err != nil {
		return err
	}
	app.session = session
	app.sessionName = ""
	return nil
}

// Run 驱动主循环直到退出；返回期间未消费的错误。
func (app *App) Run() error {
	fmt.Fprint(app.out, hideCursor)
	defer func() {
		fmt.Fprint(app.out, showCursor)
		app.term.Close()
	}()

	app.printBanner()
	if app.resumeAtStart {
		app.openResumePicker()
	}

	keyEvents := make(chan terminalinput.Key, 32)
	go func() {
		for {
			key, err := app.term.ReadKey(app.inputCtx)
			if err != nil {
				close(keyEvents)
				return
			}
			select {
			case keyEvents <- key:
			case <-app.inputCtx.Done():
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	for !app.quit {
		app.render()
		select {
		case key, ok := <-keyEvents:
			if !ok {
				app.quit = true
				continue
			}
			app.handleKey(key)
		case event := <-app.events:
			app.handleEvent(event)
		case request := <-app.gateReq:
			app.confirm = &request
		case task := <-app.uiTasks:
			task()
		case <-winch:
			app.paint.reset()
		case <-app.loopCtx.Done():
			app.quit = true
		}
	}
	// 退出前清掉活动区并保留历史。
	app.eraseActive()
	fmt.Fprintln(app.out, styleDim+"bye"+styleReset)
	return app.exitErr
}

// printBanner 执行 printBanner 对应的包内处理，并遵循调用方提供的输入契约。
func (app *App) printBanner() {
	branch := ""
	if app.gitBranch != "" {
		branch = " (" + app.gitBranch + ")"
	}
	profile := app.session.Profile()
	info, _ := app.profiles.Get(profile)
	app.commit(
		styleBold+"sn-code"+styleReset+styleDim+" · "+profile+" · "+info.Model+styleReset,
		styleDim+app.cwd+branch+styleReset,
		styleDim+"type a task, /help for commands, esc to interrupt"+styleReset,
	)
}

// commit 把行追加进历史区：擦除活动区 → 打印 → 记入历史缓冲。
// 参数 lines 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) commit(lines ...string) {
	app.eraseActive()
	for _, line := range lines {
		fmt.Fprintln(app.out, line)
		app.history = append(app.history, line)
	}
	if len(app.history) > historyLimit {
		app.history = app.history[len(app.history)-historyLimit:]
	}
	app.paint.reset()
}

// eraseActive 清除当前活动区帧（光标回到帧首）。
func (app *App) eraseActive() {
	if app.paint.lastLines > 1 {
		fmt.Fprintf(app.out, "\x1b[%dA", app.paint.lastLines-1)
	}
	fmt.Fprint(app.out, "\r"+eraseBelow)
	app.paint.reset()
}

// render 组装视图状态并重绘活动区。
func (app *App) render() {
	width, _ := app.term.Size()
	view := viewState{
		width:   width,
		busy:    app.isBusy(),
		status:  app.status,
		editor:  app.editor,
		confirm: app.confirmCall(),
		picker:  app.picker,
		footer:  app.footerText(width),
	}
	if app.streaming.Len() > 0 {
		wrapped := wrapLines(strings.TrimRight(app.streaming.String(), "\n"), width)
		if len(wrapped) > streamingTailLines {
			view.streamMore = true
			wrapped = wrapped[len(wrapped)-streamingTailLines:]
		}
		view.streamTail = wrapped
	}
	_ = app.paint.repaint(buildFrame(view))
}

// confirmCall 执行 confirmCall 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 *codingagent.ToolCallInfoForGate 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (app *App) confirmCall() *codingagent.ToolCallInfoForGate {
	if app.confirm == nil {
		return nil
	}
	call := app.confirm.call
	return &call
}

// footerText 执行 footerText 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 width 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (app *App) footerText(width int) string {
	branch := ""
	if app.gitBranch != "" {
		branch = " (" + app.gitBranch + ")"
	}
	base := filepath.Base(app.cwd) + branch
	// 会话已命名时 footer 显示名称，否则退回 ID 前缀。
	idShort := app.sessionName
	if idShort == "" {
		idShort = app.session.SessionID()
		if len(idShort) > 11 {
			idShort = idShort[:11]
		}
	}
	totals := app.session.Totals()
	cache := totals.CacheReadTokens + totals.CacheWriteTokens
	estimate, window := app.session.ContextEstimate()
	contextPart := "~" + compactTokens(estimate) + "/" + compactTokens(window)
	if window > 0 {
		contextPart += fmt.Sprintf(" %d%%", estimate*100/window)
	}
	info, _ := app.profiles.Get(app.session.Profile())
	spinner := " "
	if app.isBusy() {
		spinner = "● "
	}
	return spinner + base + " · " + idShort + " · " + contextPart +
		" · ↑" + compactTokens(cache) + " · " + app.session.Profile() + "/" + info.Model
}

// compactTokens 执行 compactTokens 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func compactTokens(value int64) string {
	switch {
	case value >= 1_000_000:
		return fmt.Sprintf("%d.%dM", value/1_000_000, value%1_000_000/100_000)
	case value >= 1_000:
		return fmt.Sprintf("%d.%dk", value/1_000, value%1_000/100)
	default:
		return fmt.Sprintf("%d", value)
	}
}

// handleKey 按当前模态分发按键。
// 参数 key 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) handleKey(key terminalinput.Key) {
	if app.confirm != nil {
		app.handleConfirmKey(key)
		return
	}
	if app.picker != nil {
		app.handlePickerKey(key)
		return
	}
	switch key.Name {
	case "":
		if key.Text != "" {
			app.editor.Insert(key.Text)
		}
	case "enter":
		app.submit()
	case "backspace":
		app.editor.Backspace()
	case "delete":
		app.editor.Delete()
	case "left":
		app.editor.Left()
	case "right":
		app.editor.Right()
	case "home":
		app.editor.Home()
	case "end":
		app.editor.End()
	case "up":
		app.editor.HistoryUp()
	case "down":
		app.editor.HistoryDown()
	case "tab":
		app.editor.Insert("  ")
	case "escape":
		if app.isBusy() {
			app.interruptRun("interrupted by user")
		}
	case "interrupt":
		app.handleInterrupt()
	case "eof":
		if app.editor.Value() == "" {
			app.quit = true
		}
	case "ctrl+l":
		app.clearScreen()
	default:
		// 未绑定键名忽略。
	}
}

// handleInterrupt 执行 handleInterrupt 对应的包内处理，并遵循调用方提供的输入契约。
func (app *App) handleInterrupt() {
	switch {
	case app.editor.Value() != "":
		app.editor.Clear()
	case app.isBusy():
		app.interruptRun("interrupted by user")
	default:
		app.quit = true
	}
}

// handleConfirmKey 执行 handleConfirmKey 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 key 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) handleConfirmKey(key terminalinput.Key) {
	request := *app.confirm
	decision := codingagent.GateDeny
	switch {
	case key.Text == "y", key.Text == "Y", key.Name == "enter":
		decision = codingagent.GateOnce
	case key.Text == "a", key.Text == "A":
		decision = codingagent.GateAlways
	default:
		decision = codingagent.GateDeny
	}
	app.confirm = nil
	request.response <- decision
}

// handlePickerKey 执行 handlePickerKey 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 key 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) handlePickerKey(key terminalinput.Key) {
	switch key.Name {
	case "up":
		app.picker.move(-1)
	case "down":
		app.picker.move(1)
	case "pageup":
		app.picker.move(-5)
	case "pagedown":
		app.picker.move(5)
	case "enter":
		picker := app.picker
		app.picker = nil
		if picker.onPick != nil {
			if err := picker.onPick(picker.options[picker.selected]); err != nil {
				app.commit(styleError + "error: " + err.Error() + styleReset)
			}
		}
	case "escape", "interrupt":
		app.picker = nil
	}
}

// submit 处理回车提交：!命令直发 shell、/命令进命令分发，其余进入 run
// 或 steering 队列。
func (app *App) submit() {
	value := app.editor.Submit()
	if value == "" {
		return
	}
	app.status = ""
	switch {
	case strings.HasPrefix(value, "!"):
		command := strings.TrimSpace(strings.TrimPrefix(value, "!"))
		if command == "" {
			return
		}
		app.commit(styleAccent + "> !" + command + styleReset)
		app.runUserCommand(command)
	case strings.HasPrefix(value, "/"):
		app.handleCommand(value)
	default:
		app.commit(styleBold + "> " + styleReset + value)
		if app.busy {
			app.session.Steer(value)
			app.status = styleDim + "queued as steering (esc to interrupt)" + styleReset
			return
		}
		app.startRun(value)
	}
}

// startRun 在独立 goroutine 驱动一次 run；esc/Ctrl+C 可取消。
// 参数 prompt 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) startRun(prompt string) {
	app.busy = true
	app.status = styleDim + "thinking…" + styleReset
	ctx, cancel := context.WithCancel(app.loopCtx)
	app.runCancel = cancel
	// busy 的清除只认 EventRunFinished：AgentSession.Run 的所有终态路径
	// 都会发出该事件，避免旁路信号与事件竞争吞掉终态标记。
	go app.session.Run(ctx, prompt)
}

// interruptRun 执行 interruptRun 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 reason 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) interruptRun(reason string) {
	if app.runCancel != nil {
		app.runCancel()
	}
	app.status = styleWarn + reason + styleReset
}

// runUserCommand 执行 runUserCommand 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 command 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) runUserCommand(command string) {
	// run 进行中也允许直发 shell；每次完成只收口自身，不恢复历史忙状态。
	app.shells++
	go func() {
		output, isErr := app.session.RunUserBash(app.loopCtx, command, true)
		app.post(func() {
			app.finishUserCommand(output, isErr)
		})
	}()
}

func (app *App) isBusy() bool {
	return app.busy || app.shells > 0
}

// finishUserCommand 只在 UI 循环中收口一次 shell 并展示输出。
func (app *App) finishUserCommand(output string, isErr bool) {
	app.shells--
	lines := wrapLines(strings.TrimRight(output, "\n"), app.width())
	if len(lines) == 0 {
		lines = []string{styleDim + "(no output)" + styleReset}
	}
	style := styleDim
	if isErr {
		style = styleError
	}
	for index, line := range lines {
		lines[index] = style + line + styleReset
	}
	app.commit(lines...)
}

// width 执行 width 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 int 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (app *App) width() int {
	width, _ := app.term.Size()
	return width
}

// post 把任务投递回 UI 循环执行。
// 参数 task 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) post(task func()) {
	select {
	case app.uiTasks <- task:
	case <-app.loopCtx.Done():
	}
}

// handleEvent 消费 AgentSession 事件并落历史区。
// 参数 event 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) handleEvent(event codingagent.Event) {
	switch event.Type {
	case codingagent.EventRunStarted:
		app.busy = true
	case codingagent.EventTextDelta:
		app.streaming.WriteString(event.Text)
	case codingagent.EventAssistantDone:
		if event.Message != nil {
			app.commitAssistant(*event.Message)
		}
		app.streaming.Reset()
	case codingagent.EventToolFinished:
		if event.Call != nil {
			app.commitToolFinished(*event.Call)
		}
	case codingagent.EventModelError:
		if event.Err != nil {
			app.commit(styleError + "⚠ " + event.Err.Message + styleReset)
		}
	case codingagent.EventRunFinished:
		wasBusy := app.busy
		app.busy = false
		app.status = ""
		if wasBusy {
			app.commit(styleDim + finishMark(event.Text) + styleReset)
		}
		app.refreshBranch("")
	case codingagent.EventNotice:
		app.commit(styleDim + event.Text + styleReset)
	case codingagent.EventToolStarted, codingagent.EventTurnStarted, codingagent.EventUserMessage:
		// 活动区即时展示由 render 完成；无需历史动作。
	}
}

// finishMark 执行 finishMark 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func finishMark(text string) string {
	switch text {
	case "completed":
		return "✔ completed"
	case "aborted":
		return "◼ interrupted"
	default:
		return "✖ run failed"
	}
}

// commitAssistant 把完成的 assistant 消息落进历史：正文原样换行，工具
// 调用显示为单行摘要。
// 参数 message 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) commitAssistant(message contract.Message) {
	if message.Content != "" {
		lines := wrapLines(strings.TrimRight(message.Content, "\n"), app.width())
		app.commit(lines...)
	}
	for _, call := range message.ToolCalls {
		app.commit(styleAccent + "▸ " + summarizeArguments(call.Name, call.Arguments) + styleReset)
	}
}

// commitToolFinished 执行 commitToolFinished 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 call 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) commitToolFinished(call codingagent.ToolCallInfo) {
	mark := styleDim + "✔" + styleReset
	if call.IsError {
		mark = styleError + "✖" + styleReset
	}
	duration := ""
	if call.DurationMS > 0 {
		duration = fmt.Sprintf(" %dms", call.DurationMS)
	}
	header := mark + " " + call.Name + duration + " " +
		styleDim + summarizeArguments(call.Name, call.Arguments) + styleReset
	lines := []string{truncateToWidth(header, app.width(), "…")}
	// 输出预览：最多 3 行，保持紧凑。
	output := strings.TrimRight(call.Content, "\n")
	if output != "" && call.Name != "write" {
		preview := wrapLines(output, app.width()-2)
		if len(preview) > 3 {
			preview = append(preview[:3], styleDim+fmt.Sprintf("… (%d more lines)", len(preview)-3)+styleReset)
		}
		for index, line := range preview {
			preview[index] = styleDim + "  " + line + styleReset
		}
		lines = append(lines, preview...)
	}
	app.commit(lines...)
}

// refreshBranch 刷新 footer 的 git 分支显示；失败静默（非 git 目录）。
// 参数 cwd 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) refreshBranch(cwd string) {
	base := cwd
	if base == "" {
		base = app.cwd
	}
	command := exec.Command("git", "-C", base, "rev-parse", "--abbrev-ref", "HEAD")
	output, err := command.Output()
	if err != nil {
		app.gitBranch = ""
		return
	}
	app.gitBranch = strings.TrimSpace(string(output))
}

// clearScreen 重置屏幕并回放最近的历史（历史缓冲有界）。
func (app *App) clearScreen() {
	app.eraseActive()
	fmt.Fprint(app.out, clearScreen)
	app.paint.reset()
	visibleLines := max(app.height()-6, 0)
	tail := app.history
	if len(tail) > visibleLines {
		tail = tail[len(tail)-visibleLines:]
	}
	for _, line := range tail {
		fmt.Fprintln(app.out, line)
	}
}

// height 执行 height 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 int 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (app *App) height() int {
	_, height := app.term.Size()
	return height
}

// handleCommand 分发斜杠命令。
// 参数 input 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) handleCommand(input string) {
	fields := strings.Fields(input)
	if len(fields) == 0 {
		return
	}
	command := fields[0]
	rest := strings.TrimSpace(strings.TrimPrefix(input, command))
	switch command {
	case "/help":
		app.commit(strings.Split(styleDim+`/model  switch API profile        /new    start a new session
/resume reopen a session          /compact  summarize history
/name <title> name the session    !<cmd>  run a shell command
esc interrupt · ctrl+c clear input · ctrl+d quit`+styleReset, "\n")...)
	case "/model":
		app.openModelPicker()
	case "/new":
		if app.isBusy() {
			app.commit(styleWarn + "a run is active; interrupt it first" + styleReset)
			return
		}
		if err := app.swapSession(func() (*codingagent.AgentSession, error) {
			return app.factory.Create(app.gate(), app.emit, "")
		}); err != nil {
			app.commit(styleError + "new session failed: " + err.Error() + styleReset)
			return
		}
		app.commit(styleDim + "started a new session" + styleReset)
	case "/resume":
		app.openResumePicker()
	case "/compact":
		app.requestCompact(rest)
	case "/name":
		if rest == "" {
			app.commit(styleWarn + "usage: /name <title>" + styleReset)
			return
		}
		if err := app.session.NameSession(rest); err != nil {
			app.commit(styleError + "name session: " + err.Error() + styleReset)
			return
		}
		app.sessionName = rest
		app.commit(styleDim + "session named " + styleReset + styleBold + rest + styleReset)
	case "/quit", "/exit":
		app.quit = true
	default:
		app.commit(styleWarn + "unknown command " + command + " (try /help)" + styleReset)
	}
}

// openModelPicker 执行 openModelPicker 对应的包内处理，并遵循调用方提供的输入契约。
func (app *App) openModelPicker() {
	ids := app.profiles.IDs()
	options := make([]pickerOption, 0, len(ids))
	current := app.session.Profile()
	selected := 0
	for index, id := range ids {
		info, ok := app.profiles.Get(id)
		if !ok {
			continue
		}
		detail := fmt.Sprintf("%s · %s · %dk ctx", info.Driver, info.Model, info.ContextWindow/1000)
		if id == current {
			detail += " · current"
			selected = index
		}
		options = append(options, pickerOption{ID: id, Label: id, Detail: detail})
	}
	if len(options) == 0 {
		app.commit(styleWarn + "no API profiles configured" + styleReset)
		return
	}
	app.picker = &pickerState{
		title:    "switch profile",
		options:  options,
		selected: selected,
		onPick: func(option pickerOption) error {
			if err := app.session.SetProfile(option.ID); err != nil {
				return err
			}
			if err := app.factory.SaveDefaultProfile(option.ID); err != nil {
				return err
			}
			app.commit(styleDim + "switched to " + option.ID + styleReset)
			return nil
		},
	}
}

// openResumePicker 执行 openResumePicker 对应的包内处理，并遵循调用方提供的输入契约。
func (app *App) openResumePicker() {
	summaries, err := app.resume()
	if err != nil {
		app.commit(styleError + "list sessions: " + err.Error() + styleReset)
		return
	}
	if len(summaries) == 0 {
		app.commit(styleDim + "no previous sessions in this directory" + styleReset)
		return
	}
	options := make([]pickerOption, 0, len(summaries))
	for _, summary := range summaries {
		label := summary.Name
		if label == "" {
			label = summary.ID
		}
		options = append(options, pickerOption{
			ID:     summary.ID,
			Label:  label,
			Detail: fmt.Sprintf("%s · %d entries", summary.Timestamp.Format("2006-01-02 15:04"), summary.Entries),
		})
	}
	app.picker = &pickerState{
		title:    "resume session",
		options:  options,
		selected: 0,
		onPick: func(option pickerOption) error {
			for _, summary := range summaries {
				if summary.ID != option.ID {
					continue
				}
				if err := app.swapSession(func() (*codingagent.AgentSession, error) {
					return app.factory.Open(app.gate(), app.emit, summary.Path)
				}); err != nil {
					return err
				}
				app.commit(
					styleDim + "resumed " + summary.Timestamp.Format("2006-01-02 15:04") + " · " +
						fmt.Sprintf("%d entries", summary.Entries) + styleReset,
				)
				return nil
			}
			return errors.New("session vanished")
		},
	}
}

// requestCompact 执行 requestCompact 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 instructions 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (app *App) requestCompact(instructions string) {
	if app.isBusy() {
		app.commit(styleWarn + "a run is active; interrupt it first" + styleReset)
		return
	}
	app.busy = true
	app.status = styleDim + "compacting…" + styleReset
	go func() {
		err := app.session.Compact(app.loopCtx, instructions)
		app.post(func() {
			app.busy = false
			app.status = ""
			if err != nil {
				app.commit(styleWarn + "compact: " + err.Error() + styleReset)
				return
			}
			app.commit(styleDim + "context compacted" + styleReset)
		})
	}()
}

// swapSession 原子替换会话：关闭旧的（flush），新的生效。
// 参数 build 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (app *App) swapSession(build func() (*codingagent.AgentSession, error)) error {
	next, err := build()
	if err != nil {
		return err
	}
	if app.session != nil {
		_ = app.session.Close()
	}
	app.session = next
	app.sessionName = next.Name()
	app.streaming.Reset()
	return nil
}
