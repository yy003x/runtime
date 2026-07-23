# Managed Run 实时输出（follow）改造说明

状态：`proposed`，尚未实施。

目标：让 `sn-cli` 的 managed Run 在保留 AgentRun、持久队列和运行产物契约的前提下，尽早显示 `run_id`，并把 Provider 执行过程实时输出到当前终端。

## 结论

- Provider 实际执行命令的即时预览由 `sn-cli profile command <profile> --mode exec` 提供，不属于本次 Runtime 改造。
- 当前实时输出缺失发生在 managed 链路：Provider stdout/stderr 会持续写入 Run 的 `output.log`，前台 `Wait()` 只轮询终态，结束后才打印最终结果。
- 推荐在 AgentRun 层增加可复用的 follow 能力，由 CLI 决定如何展示；不要把 managed 调用改成 direct invocation。
- 实现 follow 前必须先把 `output.log` 收敛为单调追加。当前日志运行中追加、结束时又被 `writeOutputLog()` 整体覆盖，直接按 byte offset tail 会重复或漏输出。

## 当前链路

```text
stdin | sn-cli session run <profile>
  -> runManagedSessionAction()
  -> Service.Run()
  -> Service.Submit()
  -> daemon dispatch
  -> runImmediate()
  -> Provider.Execute()
  -> runProviderSink.Stdout/Stderr()
  -> output.log

前台：Service.Wait() 轮询 status，终态后才返回
```

当前行为：

- `Submit()` 能立即得到 `run_id`、`run_dir` 和队列位置，但 `Run()` 将 `Submit + Wait` 封装在一起，CLI 在结束前拿不到这些信息。
- `runProviderSink` 已经按 chunk 接收 stdout/stderr，并在 Provider 运行期间追加到 `output.log`。
- `Wait()` 每 100ms 轮询 status，不读取或转发新增日志。
- `task watch` 也只在终态或超时后一次性返回 status + logs，不是真正的 follow。
- Provider 结束后，`writeOutputLog()` 使用 `os.WriteFile` 重写完整日志；该行为与增量 follow 冲突。

## 目标行为

### 顶层 managed profile

以下调用保持 managed AgentRun，不改变现有 direct/managed 路由：

```bash
sn-cli cx "处理任务"
printf '处理任务' | sn-cli cx
```

推荐默认行为：

1. `Submit()` 返回后立即向 stderr 输出一次 `[run:<run_id>] <run_dir>`；如处于队列中，可同时显示 queue position。
2. Provider 执行期间，把 `output.log` 的新增 stream 内容实时转发到 stderr。
3. 完成后，stdout 仍只输出一次现有 final text；不得因 follow 再重复打印 final text。
4. direct invocation，例如 `sn-cli cx`、`sn-cli cx --help`、`sn-cli cx exec ...`，行为完全不变。

顶层 profile 不新增 `--follow` 参数：现有路由契约规定首个 flag 属于 direct passthrough，把 `sn-cli cx --follow` 改成 runtime flag 会破坏 direct/managed 边界。

### Namespace 命令

建议为显式 namespace 增加可选 follow：

```bash
sn-cli task run -c cx --follow "处理任务"
sn-cli turn run -c cx --session-id <id> --follow "继续"
sn-cli task watch --run-id <id> --follow
```

- `run --follow`：进度写 stderr，最终结构化 JSON 继续写 stdout。
- `watch --follow`：可附着到已有 Run，终态前持续输出新增日志。
- `submit` 保持 pending-only，不接受 `--follow`。
- 不带 `--follow` 的 namespace 命令保持当前输出，避免影响机器调用方。

## 推荐实现

### P1：稳定增量日志

主要文件：

- `internal/agentrun/service.go`
- `internal/agentrun/native.go`（确认共享 `writeOutputLog()` 的非流式调用不回归）
- `internal/agentrun/service_test.go`

改造要点：

1. Provider 启动前初始化一次 `output.log` header。
2. Provider 运行中只追加 stdout/stderr stream record。
3. Provider 结束时只追加缺失的 fallback 输出和 `returncode`，不再覆盖已经写入的日志。
4. 记录各 stream 是否已经通过 sink 写入，避免又从 `provider.Result` 重复追加。
5. 保持现有 `output.log` 路径与可读格式，不新增第二套日志事实源。

阶段门禁：日志必须满足 append-only；否则不得进入 byte-offset follow 实现。

### P2：增加 AgentRun follow 原语

主要文件：

- `internal/agentrun/queue.go`，或新增职责单一的 `internal/agentrun/follow.go`
- `internal/agentrun/queue_test.go`

建议提供一个基于 `run_type + run_id` 的 follow 方法：

- 从当前 byte offset 读取 `output.log` 新增内容。
- follow cursor 从 `--- stream ---` marker 之后开始，不能把可能含原始 argv 的 header 转发到终端。
- 日志尚未创建时继续等待，不把 queued 状态当成错误。
- status 变化时只通知一次，避免重复刷屏。
- 看到终态后再 drain 一次日志，确保最后一个 chunk 不丢失。
- follow 客户端退出或 writer 失败时，只结束附着，不回滚或重跑 daemon 中的 Run。
- 保持 `result.json` 为完成事实源，实时 stdout 不作为成功判定。

不要让 CLI 直接拼接 `~/.sn/runs/...` 路径；路径解析和增量读取仍由 `agentrun.Service` 持有。

### P3：接入 CLI

主要文件：

- `internal/cli/root.go`
- `internal/cli/runtime_commands.go`
- `internal/cli/root_test.go`
- `internal/cli/runtime_commands_test.go`

改造要点：

1. 顶层 managed profile 从当前 `Service.Run()` 调整为 `Submit -> 立即打印 run 信息 -> Follow/Wait -> 打印一次 final text`。
2. `task/turn run` 和 `task/turn watch` 解析 `--follow`，复用同一 follow 原语。
3. follow 内容统一写 stderr，保留 stdout 的 final text / JSON 兼容性。
4. 不把原始 argv、profile env、prompt 或 secret 写入新增状态输出；实际命令预览继续复用 `config command` 的脱敏结果。
5. 不改变 signal、cancel、queue timeout 和 Provider deadline 的既有语义；若实现中发现它们必须一起变化，停止并单独设计。

### P4：同步契约与使用方

Runtime 同一变更需更新：

- `docs/cli-routing-contract.md`
- `docs/integration-arch.md`
- `README.md`
- `sn-cli --help`

Runtime 安装态更新后，Workbench 的 `commit` 再单独完成：

1. 执行前调用 `sn-cli profile command cx --mode exec`，立即打印脱敏命令。
2. 继续使用 `session run` managed path；它将自动获得实时 follow。

## 验收标准

1. managed profile 提交后，无需等待 Provider 完成即可看到 `run_id`。
2. fixture Provider 分两次输出并在中间阻塞时，第一段必须在进程结束前出现在终端。
3. queued、running、result_pending 和终态切换不会造成日志重复。
4. Provider 最后一个无换行 chunk 也能在终态 drain 时显示。
5. stdout 兼容：profile final text 只出现一次；namespace JSON 仍可被解析。
6. `output.log` 内容完整、单调追加，follow 不改变 `request.json`、`status.json`、`events.jsonl`、`result.json` 契约。
7. `task submit`、direct invocation、tmux/session attach 行为不变。
8. follow 断开不影响 daemon 中 Run 的继续执行和最终产物。

## 测试与验证

优先增加不依赖真实模型的 fixture 测试：

- `internal/agentrun/queue_test.go`：queued 后附着、增量读取、终态 drain、无重复、follow 断开。
- `internal/cli/root_test.go`：run 信息先出现、stream 早于完成、final text 只输出一次、direct 路由不变。
- `internal/cli/runtime_commands_test.go`：`--follow` parser、`submit` 拒绝 follow、stdout/stderr 分离。

建议验证顺序：

```bash
gofmt -w <changed-go-files>
go test ./internal/agentrun ./internal/cli -count=1
go test ./... -count=1
go vet ./...
make sn-cli-test
git diff --check
```

修改 CLI 路由或公开参数时，上述全量门禁必须与 `docs/cli-routing-contract.md` 同步通过。

## 非目标与停止条件

非目标：

- 不通过 direct invocation 绕过 AgentRun、queue 或 `result.json`。
- 不新增 WebSocket、SSE 或远程日志协议；本阶段只解决本机 CLI follow。
- 不让 `sn-cli` 自动打印 Provider 命令；调用方继续使用现有脱敏 preview。
- 不改变 artifact schema、安装同步策略或 active config 来源。

停止条件：

- 实现需要破坏 direct/managed 路由或 stdout 的既有机器契约。
- 无法保证 `output.log` append-only，或 follow 会重复/丢失终态输出。
- 需要同时改变 cancel/signal 生命周期才能工作。
- 定向测试出现数据竞争、跨 Run 串流或 secret 暴露风险。
