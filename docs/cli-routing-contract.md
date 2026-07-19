# sn-cli 路由契约

本文是 `sn-cli` 命令语法、profile 分流与参数边界的规范源。入口实现、帮助文案、README 和测试不得与本文冲突。

## 1. 统一语法

```text
sn-cli <namespace> <action> [named-options...] [prompt...] [-- raw-cli-args...]
sn-cli <profile> [prompt...] [-- raw-cli-args...]
```

- `prompt` 是唯一允许不带 key 的业务数据，多个 positional token 以空格连接。引号只是 shell 的分组语法，不属于 `sn-cli` 协议。
- config 统一使用 `-c/--config`。
- lifecycle ID 统一使用 `--run-id` 或 `--loop-id`，不接受 positional ID。
- prompt 文件只使用 `--prompt-file`，不存在 `--prompt`。
- `--` 之后的全部参数只属于目标 CLI，`sn-cli` 不再解析。
- 旧 `prompt` 命令、`prune` 命令、`--profile`、下划线参数和旧 positional ID 均无兼容入口。

prompt 来源为 positional、`--prompt-file`、stdin 三选一；同时提供多个来源必须报错。session 可以不提供 prompt，此时只启动交互 CLI。

## 2. 顶层解析优先级

第一个参数按以下顺序解析：

1. `-h`、`--help` 等全局 flag。
2. `help`、`version`、`profiles`、`config`、`doctor`、`daemon`、`task`、`turn`、`runs`、`loop`、`capabilities`、`tools`、`session`、`history`、`command`、`clean`、`update` 等内建命令。
3. 从 `~/.sn/configs/*.json` 解析 config ID、alias 或 preset ID。
4. 均未命中时返回 `unknown command`，不得猜测 Provider 或静默降级。

config ID、alias 和 preset ID 不得与内建命令重名，配置加载阶段必须拒绝冲突。

`providers` 是 `profiles` 的 legacy alias，`upgrade` 是 `update` 的 legacy alias。二者继续可用并与正式命令一同列入保留字，但新文档和脚本只使用正式命令。除这两个明确登记的别名外，不新增隐式兼容入口。

## 3. Command Profile 分流

当 config 是 `type=cli` 且 `cli.executor=command` 时，只检查第一个 profile 参数：

| 调用形式 | 路由 | 参数处理 | Artifact |
| --- | --- | --- | --- |
| `sn-cli <profile>` | direct interactive | 无额外参数 | 无 run artifact；无逻辑 Session |
| `sn-cli <profile> -x ...` | direct passthrough | 参数直接传给目标 CLI | 无 run artifact；无逻辑 Session |
| `sn-cli <profile> --long ...` | direct passthrough | 参数直接传给目标 CLI | 无 run artifact；无逻辑 Session |
| `sn-cli <profile> -- ...` | direct passthrough | 移除 `--` 后原样传递 | 无 run artifact；无逻辑 Session |
| `sn-cli <profile> <text> ...` | managed prompt | 普通文本由 AgentRun 解析 | 有 |
| `stdin | sn-cli <profile>` | managed prompt | stdin 由 AgentRun 解析 | 有 |

direct 调用只使用 `cli.command.args`、model 和 env，不加入 `managed_args`。顶层 direct 不记录逻辑 Session；只有显式 `session exec` 才记录 `capture_quality=metadata_only` 的执行元数据。managed 调用的目标 argv 顺序固定为：

```text
binary + command.args + model + raw-cli-args + managed_args
```

顶层 `sn-cli <profile> <prompt>` 的 stdout 优先返回本次 Provider 的真实 final text，使 `cx`/`cc` 保持与原生非交互 CLI 一致；只有幂等复用等本次没有 Provider 输出的情况才回退到 `result.json.summary`。run ID 和目录只写 stderr，结构化结果仍以 artifact 为准。

Provider 差异必须由 config 表达，不得在入口硬编码 `cx -> exec` 或 `cc -> -p`。

```bash
sn-cli cx
sn-cli cc
sn-cli cx "hi"
printf 'hi' | sn-cli cx
sn-cli cx --help
sn-cli cx -- exec "hi"
sn-cli cx "hi" -- --skip-git-repo-check
```

`sn-cli cx exec "hi"` 表示 managed prompt `exec hi`。原生 Codex `exec` 必须写为 `sn-cli cx -- exec "hi"`。

### 3.1 Command 环境契约

direct、managed 与 session 必须使用同一份 `cli.command` 环境配置。子进程环境顺序固定为：继承当前环境、应用 `env_unset`、应用 `env_passthrough`、应用 `env`、注入 runtime 环境。后写入的值优先。

- `env` 用于 profile/preset 固定覆盖，值支持环境变量展开。
- `env_passthrough` 用于把当前 `sn-cli` 进程变量显式传入 tmux 子进程。
- `env_unset` 用于删除继承变量；preset 追加写法为 `env_unset_append`。
- 同一个变量不得同时出现在 `env_unset` 与 `env`/`env_passthrough`。
- config 不得保存 secret，只能传递或删除 secret 对应的环境变量。

默认 `cx`/`cc` 继承当前 shell 的 `CODEX_HOME` / `CLAUDE_CONFIG_DIR`。多账号目录应通过 shell 环境或 profile preset 表达，不得在 CLI 路由层按 profile ID 硬编码。

## 4. Namespace 契约

```bash
sn-cli task run -c cx "hi"
sn-cli task submit -c cx --queue-timeout-seconds 3600 "后台执行"
sn-cli turn submit -c cx --session-id <id> "后台继续"
sn-cli session run -c cx "一次性但需要会话记录的任务"
sn-cli session exec -c cx -- --help
sn-cli task status --run-id <id>
sn-cli loop status --loop-id <id>
sn-cli runs list --active --project <project> --limit 20
sn-cli runs reconcile --dry-run

sn-cli session start -c cx "hi" -- --no-alt-screen
sn-cli session list
sn-cli session send --run-id <id> "继续"
sn-cli session logs --run-id <id> --tail 200
sn-cli session stop --run-id <id>

sn-cli history create --session-id <id> --project <project> --runtime api --profile ba
sn-cli history list
sn-cli history show --session-id <id>
sn-cli history messages --session-id <id> --after-seq 0
sn-cli history events --session-id <id> --after-seq 0
sn-cli history configure --session-id <id> --runtime cli --profile cx --retention pinned
sn-cli history delete --session-id <id>
sn-cli history rebuild

sn-cli config command -c cx-spark
sn-cli config command -c cx-spark --json

sn-cli clean
sn-cli clean --apply
```

`session run` 创建 `ephemeral` 逻辑 Session 并执行一次 managed Run；`session exec` 在前台直连原生 CLI，只记录 metadata，显式请求 `record_mode=full` 会被拒绝；`session start` 创建 standard 逻辑 Session 并复用同一份 command config 启动 tmux，不读取单独的 `tcx/tcc` 配置。tmux 启动成功表示 pane 已稳定；存在首个 prompt 时，还要求粘贴和 Enter 成功并记录 `prompt.submitted`，不表示模型任务已完成。

`config command` 只读解析 CLI profile 的 managed argv，不启动 Provider，也不输出 profile env 值。文本模式输出可供 shell 阅读的脱敏命令；`--json` 输出 `argv` 与 `command`，敏感 flag、URL 凭据、敏感 query 和当前环境中的 secret 值必须替换为 `[REDACTED]`。

`session list/status/send/...` 管理 tmux 活动执行；`history ...` 管理跨 API、CLI、TTY、tmux 的逻辑 Session。普通 `task run`、顶层 managed profile 和 `POST /v1/runs` 只写 Run artifact，不隐式创建 Session；需要记录时使用 `session run`、Session HTTP API，或用 `--session-id` 关联既有 Session。`turn run` 强制要求 `--session-id`。`--record-mode`、`--retention` 只有存在显式 Session intent 时有效。

`task run` 同步提交到本机持久 FIFO 并等待终态；`task submit` 只提交并立即返回 `pending`。`--queue-timeout-seconds` 限制等待调度的时间，不替代 Provider 的 `--deadline-seconds`。`runs list` 是跨进程查询 queued/active/terminal Run 的公共入口；`runs reconcile` 只修复 daemon 异常退出遗留的 claim，已有终态 artifact 绝不能重跑。队列容量或并发不足必须排队或返回 `queue_full`，不得再以 `max_concurrency=1 reached` 拒绝正常提交。

## 5. 变更门禁

修改顶层命令、参数名称、`--` 语义、prompt 来源或 direct/managed/session 边界时，必须在同一变更中：

1. 更新本文并说明不兼容影响。
2. 更新 CLI 路由、parser 和 artifact 测试。
3. 更新 `sn-cli --help` 与 README。
4. 同步更新 `docs/integration-arch.md`。
5. 运行 `go test ./...`、`go vet ./...` 和 `make sn-cli-test`。

只修改实现而不更新契约和测试，视为未完成。
