# sn-cli 详细使用手册

本文是 SN Runtime 公开 CLI 的详细用户手册，覆盖 `sn-cli` 当前实现的命令、
子命令、参数、输入输出、状态、典型场景和示例。

每条命令的详细说明统一按「语法 / 作用 / 参数 / 示例 / 适用场景」组织；需要快速
定位某条命令时，先看 [命令速查表](#命令速查表)，再跳到对应编号章节。

本文只描述当前协议。所有入口、配置和 machine output 都按本文的完整结构严格校验；
架构约束和内部一致性要求以 `docs/runtime-contract.md` 的唯一契约为准。

## 目录

- [命令速查表](#命令速查表)
- [1. 应该使用哪个入口](#1-应该使用哪个入口)
- [2. 完整命令树](#2-完整命令树)
- [3. 全局调用规则](#3-全局调用规则)
- [4. Runtime Home 与配置](#4-runtime-home-与配置)
- [5. Profile 管理和执行入口](#5-profile-管理和执行入口)
- [6. Session](#6-session)
- [7. Tmux](#7-tmux)
- [8. Agent](#8-agent)
- [9. Durable Run](#9-durable-run)
- [10. Server](#10-server)
- [11. 安装与本地源码更新](#11-安装与本地源码更新)
- [12. HTTP API](#12-http-api)
- [13. 常见完整工作流](#13-常见完整工作流)
- [14. 常见错误与排查](#14-常见错误与排查)
- [15. 内部入口和协议边界](#15-内部入口和协议边界)
- [16. 相关契约文档](#16-相关契约文档)

## 命令速查表

下表按 namespace 列出 `sn-cli` 的全部公开命令，每条一行，便于快速定位。「记录」列
同时区分 canonical state 与 best-effort local log：`cli-log/api-log` 只是可丢失执行
诊断，`audit` 是脱敏控制面审计，`session` 写文件型 Session，`durable` 创建 SQLite
Durable Run，`—` 表示纯查询。queue submit 当下不写 execution log，worker 真正执行
后才写，但相关 mutation 仍写 audit。

### 执行与 Profile

| 命令 | 作用 | 记录 | 示例 |
|---|---|:---:|---|
| `sn-cli <cli-profile>` | CLI interactive direct | cli-log | `sn-cli cx` |
| `sn-cli exec <cli-profile>` | CLI non-interactive 一次执行 | cli-log | `sn-cli exec cx "回复OK"` |
| `sn-cli req <api-profile>` | 一次 API request | api-log | `sn-cli req api-cx "回复OK"` |
| `sn-cli profile list` | 列出所有 Profile 的 ID 与类型 | — | `sn-cli profile list` |
| `sn-cli profile show <id>` | 查看 Profile 实际配置 | — | `sn-cli profile show cx` |
| `sn-cli profile check [id]` | 校验 Profile 结构与分流 | — | `sn-cli profile check` |

### session

| 命令 | 作用 | 记录 | 示例 |
|---|---|:---:|---|
| `session exec <cli-profile>` | 同步执行并记录 CLI Turn | session + cli-log + audit | `sn-cli session exec cx "分析当前仓库"` |
| `session req <api-profile>` | 同步执行并记录 API Turn | session + api-log + audit | `sn-cli session req api-cx "分析这段内容"` |
| `session exec\|req <profile> --queue` | 创建 queued Session Turn | durable + audit；worker 执行时 log | `sn-cli session exec cx-deep --queue "后台执行"` |
| `session open <cli-profile>` | 创建/绑定 tmux-backed durable Session console | session + durable + tmux + audit | `sn-cli session open cx --cwd "$PWD"` |
| `session send` | 向 console 提交下一轮输入 | audit；console 消费后创建 durable Run | `sn-cli session send --session-id <id> "继续"` |
| `session attach\|interrupt\|close` | 连接、取消 active Run、关闭 console | audit + durable control / tmux | `sn-cli session close --session-id <id>` |
| `session close-all` | 关闭全部 Session console，保留 Session fact | audit + durable control / tmux | `sn-cli session close-all` |
| `session list` | 列出或按状态过滤 Session | — | `sn-cli session list --state blocked` |
| `session show` | 查看 Session 状态与事实 | — | `sn-cli session show --session-id <id>` |
| `session messages` | 读取消息历史，支持增量 | — | `sn-cli session messages --session-id <id> --after-seq 10` |
| `session events` | 读取生命周期与 Execution 事件 | — | `sn-cli session events --session-id <id>` |
| `session logs` | 查看最近活动（默认 tail 120） | — | `sn-cli session logs --session-id <id>` |
| `session executions` | 列出 Session 的全部 Execution | — | `sn-cli session executions --session-id <id>` |
| `session execution` | 查看单个 Execution 详情 | — | `sn-cli session execution --session-id <id> --execution-id <id>` |
| `session reconcile` | 收口 blocked / unknown Execution | audit | `sn-cli session reconcile --session-id <id> --terminate` |
| `session configure` | 修改 Session retention | audit | `sn-cli session configure --session-id <id> --retention pinned` |
| `session export` | 导出 Session 到文件 | — | `sn-cli session export --session-id <id> --output ./out.json` |
| `session delete` | 删除非活跃 Session（移入 trash） | audit | `sn-cli session delete --session-id <id>` |
| `session gc` | 回收过期 ephemeral Session | audit | `sn-cli session gc --older-than-hours 72 --apply` |

### tmux

| 命令 | 作用 | 记录 | 示例 |
|---|---|:---:|---|
| `tmux open` | 创建一个 managed 交互窗口 | cli-log + audit | `sn-cli tmux open cx "打开长期任务"` |
| `tmux list` | 列出 managed 窗口 | — | `sn-cli tmux list` |
| `tmux show` | 查看窗口身份与状态 | — | `sn-cli tmux show --tmux-id <id>` |
| `tmux send` | 向窗口发送输入 | audit | `sn-cli tmux send --tmux-id <id> "继续"` |
| `tmux attach` | 进入 managed 窗口 | audit | `sn-cli tmux attach --tmux-id <id>` |
| `tmux interrupt` | 对 running 窗口发送 C-c | audit | `sn-cli tmux interrupt --tmux-id <id>` |
| `tmux stop` | 按身份停止窗口 | audit | `sn-cli tmux stop --tmux-id <id>` |
| `tmux stop-all` | 停止全部 raw window | audit | `sn-cli tmux stop-all` |

### agent

| 命令 | 作用 | 记录 | 示例 |
|---|---|:---:|---|
| `agent <api-profile>` | 同步运行 API-only model/tool 循环 | durable + audit + 每轮 api-log | `sn-cli agent api-cx "审查并报告"` |
| `agent <api-profile> --queue` | 创建 queued Agent Run | durable + audit；worker 每轮 api-log | `sn-cli agent api-cx --queue "后台审查"` |

### run

`run` 不接受 fresh submission，只查询或控制由带 `--queue` 的 Session/Agent 入口
或同步 `agent` 创建的 Durable Run；`run retry` 是基于已有终态 Run 的控制动作。

| 命令 | 作用 | 记录 | 示例 |
|---|---|:---:|---|
| `run get` | 获取完整 Run Record | — | `sn-cli run get --run-id <id>` |
| `run list` | 列出或按状态/kind 过滤 Run | — | `sn-cli run list --state failed` |
| `run result` | 读取当前 result/error/state | — | `sn-cli run result --run-id <id>` |
| `run events` | 读取持久化事件 | — | `sn-cli run events --run-id <id>` |
| `run watch` | 流式追踪事件直到终态 | — | `sn-cli run watch --run-id <id>` |
| `run cancel` | 取消 Run（queued/paused/running） | audit | `sn-cli run cancel --run-id <id>` |
| `run resume` | 恢复 paused Run | audit | `sn-cli run resume --run-id <id> --input-file ./resume.json` |
| `run retry` | 重试终态 Run | durable + audit | `sn-cli run retry --run-id <id>` |
| `run reconcile` | 收口 unknown outcome Run | audit | `sn-cli run reconcile --run-id <id>` |
| `run gc` | 永久回收过期终态 Run | audit | `sn-cli run gc --older-than 720h --apply` |

### doctor 与 server

| 命令 | 作用 | 记录 | 示例 |
|---|---|:---:|---|
| `server info` | 显示 Runtime/Profile/数据库信息 | — | `sn-cli server info` |
| `doctor` | Runtime/Profile/Tool/日志/tmux 本机检查 | audit | `sn-cli doctor` |
| `server start` | 启动 worker + HTTP（后台） | audit | `sn-cli --json server start` |
| `server status` | 查看 server 运行状态 | — | `sn-cli server status` |
| `server stop` | 停止 server（幂等） | audit | `sn-cli server stop` |
| `server update` | 检查/下载/激活新版本 | audit | `sn-cli server update --check` |
| `server upgrade-check` | 升级前 preflight 检查 | audit | `sn-cli server upgrade-check` |

### 其他

| 命令 | 作用 | 示例 |
|---|---|---|
| `help` / `-h` / `--help` | 显示根命令帮助 | `sn-cli --help` |
| `version` / `--version` | 显示构建版本 | `sn-cli --version` |

> 内部入口 `sn-cli __sn_tmux_helper` 与 `sn-cli server upgrade-activate` 不属于日常
> 用户 API，见 [§15. 内部入口和协议边界](#15-内部入口和协议边界)。

## 1. 应该使用哪个入口

| 目标 | 推荐入口 | canonical / diagnostic | 说明 |
|---|---|---:|---|
| 打开 Codex/Claude 交互 TUI | `sn-cli <cli-profile-id>` | 无 / cli-log | bare CLI Profile 固定 direct |
| 临时覆盖模型或 effort | `sn-cli <cli-profile> --model ... --effort ...` | 无 / cli-log | typed 参数优先于 Profile 配置 |
| 一次性运行 CLI 并退出 | `sn-cli exec <cli-profile> ...` | 无 / cli-log | Codex 映射为 `exec`，Claude 映射为 `-p` |
| 调用一次模型 API | `sn-cli req <api-profile> ...` | 无 / api-log | 只执行一次 Provider call |
| 保存一轮 CLI 会话历史 | `sn-cli session exec <cli-profile> ...` | session / cli-log | managed non-interactive Turn |
| 保存一轮 API 会话历史 | `sn-cli session req <api-profile> ...` | session / api-log | 单次 API Turn |
| 后台执行并保存会话 | `session exec\|req <profile> --queue ...` | durable / worker log | 提交 Durable Session Run |
| 可 attach 的长期 Session | `session open <cli-profile> ...` | session + durable + tmux | 每个已消费 prompt 一个 Durable Run |
| 保留可 attach 的长期 TUI | `sn-cli tmux open ...` | tmux / cli-log | 独立 Tmux window manager |
| 自动执行 model/tool 循环 | `sn-cli agent <api-profile> ...` | Durable Run / 每轮 api-log | 默认同步；`--queue` 只入队 |
| 查询和控制后台任务 | `sn-cli run ...` | — | 不接受 fresh submission |
| 启动 worker 和 HTTP 服务 | `sn-cli server start` | 服务状态 | 返回可供第三方托管的 PID |

几个最重要的边界：

- namespace 决定执行语义，Profile `type=cli|api` 做严格配对校验。
- bare Profile、`exec`、`req` 都不创建 Session 或 Durable Run。
- 所有实际 Profile execution 会尝试写本地 execution log；它不是 canonical 记录。
- Profile ID 紧跟拥有它的 namespace/action；option 位于其后；input 必须最后。
- `profile` 只提供 `list|show|check`，不执行 Profile。
- 当前 contract 不提供旧入口 alias、兼容 shim 或迁移版本。
- Session 自己管理 Turn、Message、Event 和 Execution，不自动执行 tool call。
- 原始 `tmux` 只管理交互窗口，不创建 Session/Run；`session open` 是显式组合入口。
- `session open` 与 `tmux open` 统一使用 `open`；action 不可省略，旧 `tmux start`
  不提供 alias。
- Agent 只接受 API Profile，负责自动 model/tool/tool-result 循环。
- `--queue` 只入队，不会自动启动 `sn-server`。

## 2. 完整命令树

```text
sn-cli
├─ <cli-profile> [options] [input]       # interactive direct
├─ exec <cli-profile> [options] [input]  # non-interactive CLI
├─ req <api-profile> [options] [input]   # one API request
├─ doctor                                # local Runtime health check
├─ profile
│  ├─ list
│  ├─ show <profile-id>
│  └─ check [profile-id]
├─ session
│  ├─ exec <cli-profile> [options] [input]
│  ├─ req <api-profile> [options] [input]
│  ├─ open <cli-profile> [options] [input]
│  ├─ send | attach | interrupt | close | close-all
│  ├─ list | show | messages | events | logs
│  ├─ executions | execution | reconcile
│  └─ configure | export | delete | gc
├─ tmux
│  ├─ open <cli-profile> [options] [input]
│  └─ list | show | send | attach | interrupt | stop | stop-all
├─ agent <api-profile> [options] [input]
├─ run
│  ├─ get | list | result | events | watch
│  └─ cancel | resume | retry | reconcile | gc
├─ server
│  ├─ info | start | status | stop | update | upgrade-check
│  └─ upgrade-activate       # 内部 activation action
├─ help | -h | --help
├─ version | --version
├─ __sn_tmux_helper          # 内部 Tmux bootstrap
└─ __sn_session_terminal_helper # 内部 Session console target
```

CLI 没有更多固定的三级 action。`profile-id`、`session-id`、`execution-id`、
`tmux-id` 和 `run-id` 是参数，不是 namespace。执行入口统一把 Profile ID 放在
namespace/action 之后，把 input 放在最后。

### ID 命名

Profile ID：

- 长度最多 128 bytes。
- 首字符必须是 ASCII 字母或数字。
- 后续字符可使用 ASCII 字母、数字、`-`、`_`、`.`。
- `.` 不能是首字符。
- 不能使用固定 namespace `doctor/exec/req/profile/session/tmux/agent/run/server/help/version`
  或管理 action `list/show/check`。

Runtime 生成的 Session 系列 ID 使用前缀加 32 位小写十六进制：

```text
session_<32hex>
turn_<32hex>
execution_<32hex>
run_<32hex>
```

`tmux_id` 使用 UUIDv7。调用方应把这些 ID 当作 opaque value，不自行构造或从底层
目录名推断。

## 3. 全局调用规则

### 3.1 Help 和 version

```bash
sn-cli
sn-cli help
sn-cli -h
sn-cli --help

sn-cli version
sn-cli --version
sn-cli --json version
```

`help`、`-h`、`--help`、`version` 和 `--version` 不接受尾随参数。

当前没有 action 级 help，下面的写法不会显示局部帮助：

```bash
sn-cli session --help
sn-cli run --help
```

需要查询完整参数时使用本文；根命令的简要入口可以通过 `sn-cli --help` 查看。

### 3.2 `--json` 必须位于第一个参数

正确：

```bash
sn-cli --json profile list
sn-cli --json session show --session-id <session_id>
sn-cli --json run get --run-id <run_id>
sn-cli --json server status
```

错误：

```bash
sn-cli profile list --json
sn-cli session list --json
```

放在 namespace、action 或 Profile ID 后面的 `--json` 不属于全局参数，会由对应
parser 当作未知参数处理。

Machine success 默认包含：

```json
{
  "schema_version": 1,
  "contract_version": 5
}
```

Machine error 写入 stderr，退出码为 `1`。Human error 为：

```text
error: <message>
```

bare CLI direct 或 `exec` 成功启动目标进程后，始终保留目标程序的原生 stdout、stderr、exit code
和 signal 语义。即使使用 leading `--json`，也不会把 Codex/Claude 的输出包装成
Runtime JSON；只有启动目标进程前发生的错误会使用 machine error envelope。

### 3.3 流式输出

以下入口输出逐行 JSON event：

```text
req <api-profile> --stream
agent <api-profile> --stream
run watch
```

CLI 的流式格式是 NDJSON。`req` 和 `agent` 成功时最后输出 final
envelope；`run watch` 在观察到 `run.settled` 后输出最终 Run envelope。

HTTP 的流式格式是 SSE，和 CLI NDJSON 不同。

### 3.4 参数值形式

不同 namespace 的参数解析形式不同：

| 范围 | `--name value` | `--name=value` |
|---|---:|---:|
| bare CLI / `exec` typed 参数 | 支持 | 支持 |
| `tmux open` typed 参数 | 支持 | 支持 |
| `tmux show/attach/interrupt/stop --tmux-id` | 支持 | 支持 |
| `req` 参数 | 支持 | 通常不支持 |
| Session 参数 | 支持 | 不支持 |
| Agent 参数 | 支持 | 不支持 |
| Run 参数 | 支持 | 不支持 |
| Server 参数 | 支持 | 不支持 |

### 3.5 `--` 与最终输入

当 prompt/input 以 `-` 开头时，使用 `--` 结束参数解析：

```bash
sn-cli exec cx -- "-这是prompt正文"
sn-cli req api-cx -- "-这是API输入"
sn-cli agent api-cx -- "-这是Agent输入"
sn-cli session req api-cx --queue -- "-这是Session输入"
```

`--` 只影响所在 action 的 parser，不是可任意插入的全局参数。

### 3.6 输入大小

| 输入 | 最大值 |
|---|---:|
| CLI Profile prompt 合并结果 | 128,000 bytes |
| `tmux open` prompt 合并结果 | 128,000 bytes |
| API Profile prompt/request | 1 MiB |
| Session input | 1 MiB |
| `tmux send` input | 1 MiB |
| Session tool result content | 1 MiB |
| Run resume input file | 1 MiB |

文本输入必须是合法 UTF-8，不能包含 NUL。

## 4. Runtime Home 与配置

默认 Runtime Home：

```text
${SN_CLI_HOME:-~/.sn}
```

本文所说的 SN root（即 `SN_ROOT` 概念）就是这个 Runtime Home；唯一公开环境变量仍是
`SN_CLI_HOME`，不增加第二个 home alias。

目录结构：

```text
bin/
  sn-cli
  sn-server
configs/
  <profile-id>.json
tools/
  <tool-name>.json
resources/
  schema/
  tmux.conf
  release.json
runtime.json
logs/
  YYMMDD/
    cli.jsonl
    api.jsonl
    audit.jsonl
  sn-server.log
sessions/
state/
  session-locks/
  session-invocations/
  session-mutations/
  session-trash-moves/
  runtime.db
  sn-server.pid
  sn-server.lease.lock
  sn-server.lifecycle.lock
  runtime.maintenance.lock
  tmux.lock
  update.json
tmp/
```

仓库 source 与 release archive payload 使用同一配置布局（archive 另有根级
`sn-cli`、`sn-server`）：

```text
configs/*.json
resources/schema/*.json
resources/tools/*.json
release/runtime.json
release/tmux.conf
release/release.json
```

activation 固定执行以下映射，不读取旧 payload shape：

```text
configs/                              → <runtime-home>/configs/
resources/tools/                      → <runtime-home>/tools/
release/runtime.json                  → <runtime-home>/runtime.json
resources/schema/                     → <runtime-home>/resources/schema/
release/tmux.conf                     → <runtime-home>/resources/tmux.conf
release/release.json                  → <runtime-home>/resources/release.json
```

active home 不反向充当 source 或 archive payload。

Profile 只有一层配置：

```text
<runtime-home>/configs/*.json
```

Profile ID 就是文件名去掉 `.json` 的部分。例如：

```text
configs/cx.json      → cx
configs/api-cx.json  → api-cx
```

不存在额外的第二层 Profile 映射。调用方也不应直接
读写 `sessions/` 和 `state/runtime.db`，应通过公开 CLI 或 HTTP。
`state/session-mutations/` 是 private `mutation_version=3` crash journal；
`state/session-trash-moves/` 是 delete/GC 使用的 private `version=1` rename
journal；`state/session-invocations/` 保存 managed CLI helper 的短生命周期
invocation manifest。新 Session 创建期间还会短暂出现带随机 nonce 的 root owner
marker。这些内容都由 Store 恢复/清理，不是公开 fact，调用方不得手工修改；
canonical Session `schema_version` 仍为 2。

固定 namespace：

```text
doctor exec req profile session tmux agent run server help version
```

Profile 管理 action：

```text
list show check
```

固定 namespace 和上述管理 action 是保留 Profile ID；其余合法名称都可以作为
Profile ID。

### 4.1 CLI Profile 配置

```json
{
  "type": "cli",
  "command": "codex",
  "args": [
    "--search"
  ],
  "env": {
    "CODEX_HOME": "${HOME}/.codex-aip"
  },
  "model": "gpt-5.6-sol",
  "effort": "xhigh",
  "prompt": "默认提示词或文件路径",
  "cwd": "/workspace"
}
```

| 字段 | 必填 | 作用 |
|---|---:|---|
| `type` | 是 | 必须是 `cli` |
| `command` | 是 | 当前 basename 只支持 `codex`、`claude` |
| `args` | 否 | 每个 JSON string 严格对应一个 argv token |
| `env` | 否 | 在继承环境上覆盖；value 为 `null` 时删除变量 |
| `model` | 否 | adapter 生成最终 model selector |
| `effort` | 否 | `low/medium/high/xhigh/max` |
| `prompt` | 否 | 文件存在则读取；不存在则作为文本 |
| `cwd` | 否 | 默认工作目录 |

execution mode 不属于 Profile 配置；配置中的 `exec` 是未知字段，会被严格拒绝。

`args`、`env` 和 `cwd` 支持 `${VAR_NAME}` 引用。被引用环境变量不存在时，执行明确
失败。Secret 不应直接写入配置，应通过环境变量传递。
所有 CLI Profile 都必须能生成 Session canonical invocation。Claude 的
`--verbose` 会把 `--output-format json` 从单个 result object 改成逐轮数组，因此
不能写入 Profile `args`，`profile check` 会 fail closed。

`args` 必须遵守一字符串一 argv token：

```json
{
  "args": [
    "--sandbox",
    "read-only",
    "-c",
    "model_verbosity=high"
  ]
}
```

不要写：

```json
{
  "args": [
    "--sandbox read-only",
    "-c model_verbosity=high"
  ]
}
```

### 4.2 API Profile 配置

```json
{
  "type": "api",
  "driver": "openai",
  "base_url": "https://example.com/provider",
  "model": "model-name",
  "headers": {
    "Authorization": "${API_KEY}"
  },
  "parameters": {
    "max_tokens": 16384,
    "temperature": 0.2,
    "stop_sequences": ["END"]
  },
  "timeout": "5m",
  "context": {
    "window_tokens": 128000,
    "reserved_output_tokens": 16384,
    "keep_recent_turns": 8,
    "summary_enabled": true
  }
}
```

当前 driver：

```text
openai
anthropic
```

API secret 通过 headers 的 `${VAR}` 引用从环境变量读取；openai driver 对裸
`Authorization` 值（不含空格）自动补 `Bearer ` 前缀，anthropic 不补。`profile show`
不会输出展开后的 secret。`endpoint` 与 `base_url` 二选一；后者按 driver 自动追加
`/v1/chat/completions` 或 `/v1/messages`，并保留已有路径前缀。

### 4.3 runtime.json

`runtime.json` 控制 Agent、scheduler、Run retention 和 tmux server mode：

```json
{
  "agent": {
    "tools": [
      "read_file",
      "list_directory",
      "web_search",
      "web_fetch"
    ],
    "workspace_roots": [],
    "max_rounds": 16,
    "max_tool_calls": 64,
    "max_total_tokens": 0,
    "max_wall_time": "15m"
  },
  "scheduler": {
    "workers": 1,
    "poll_interval": "250ms"
  },
  "run": {
    "settled_retention": "168h"
  },
  "tmux": {
    "server_mode": "default"
  }
}
```

允许的 builtin tool：

```text
read_file
list_directory
write_file
```

默认启用两个只读 builtin 与两个只读 MCP tool。`write_file` 必须显式配置；其它
名称必须在 active `tools/<name>.json` 中存在，否则 Agent bootstrap fail closed。

### 4.4 Tool Catalog

当前 release 内置交付：

| local tool | remote MCP tool | endpoint | auth env |
|---|---|---|---|
| `web_search` | `web_search_prime` | `https://open.bigmodel.cn/api/mcp/web_search_prime/mcp` | `Z_AI_API_KEY` |
| `web_fetch` | `webReader` | `https://open.bigmodel.cn/api/mcp/web_reader/mcp` | `Z_AI_API_KEY` |

两份完整 schema 位于 source/payload `resources/tools/web_search.json`、
`resources/tools/web_fetch.json`，安装后位于 `${SN_CLI_HOME}/tools/`。manifest 中只保存
`Authorization: Bearer ${Z_AI_API_KEY}` 引用，调用时才从环境展开，不保存明文。
文件名必须与 `name` 相同；接受 `schema_version=1`、`executor.type=mcp`，
`effect` 为 `read_only`/`write_local`/`write_external` 三档之一（写副作用需声明
`risk`）。`runtime.json` 决定启用哪些 builtin/manifest tool。

调用示例：

```bash
export Z_AI_API_KEY='...'
sn-cli agent api-cc \
  "查找 Codex CLI 最新版本，并阅读官方发布页面后总结主要更新。"
```

`agent` 会把启用的 definitions 注入每一轮 model request，并自动执行 model 返回的
tool call。`req` 仍只有一次 Provider call；`session req` 仍不自动执行 tool。
每次 MCP 执行不 retry、不跟随 redirect；失败作为 `is_error=true` 的 tool result
返回下一轮模型。`doctor` 会检查已启用 tool 的环境变量，但不访问远端。

### 4.5 本地 execution、audit 与 server log

只有携带 Profile ID 并进入真实执行边界的调用才写日志：CLI 在最终 invocation
准备 launch 时写 `cli.jsonl`；API 在 Provider driver 调用 `http.Client.Do` 后写
`api.jsonl`。`profile list/show/check`、Session/Run/Tmux 查询和控制、前置校验失败、
queue submit 都不写这两类 execution log。queued Session/Agent 由 worker 真正执行
时写；Agent 每个 model round 各写一条。MCP HTTP 不写 `api.jsonl`；tool call/result 由 Durable Run 的
effect/event evidence 记录。`session logs` 是 canonical Session activity，和这里的
execution log 不是同一个概念。

目录按本地日期 `YYMMDD` 划分：

```text
${SN_CLI_HOME}/logs/260804/cli.jsonl
${SN_CLI_HOME}/logs/260804/api.jsonl
${SN_CLI_HOME}/logs/260804/audit.jsonl
${SN_CLI_HOME}/logs/sn-server.log
```

`audit.jsonl` 记录脱敏控制面元数据：CLI 的 `doctor`、Session/Tmux/Run/server
mutation、Session/Agent 执行提交，以及所有 HTTP access。每行只保存规范化 action、
稳定 target ID、outcome、canonical error identity 或 HTTP status；不保存任意 argv、
query、prompt/send 内容、request/response body、error message、header、cookie 或
resolved secret。只读 CLI query 不写 audit。`sn-server.log` 是后台 server process 的
stdout/stderr，不是 JSONL。

三类日志都只是 best-effort observer，不参与 Session/Run transaction、replay 或
terminal 判定；写入失败不改变业务结果。`doctor` 会主动写一条 audit sink probe，
因此能显式报告日志目录不可写。

CLI 行保持紧凑结构：

```json
{"time":"2026-08-04 10:40:23","namespace":"exec","profile":"cx","source":"sn-cli exec cx 简单介绍下自己","command":"CODEX_HOME=${HOME}/.codex-aip && cd /workspace && /opt/homebrew/bin/codex exec --ephemeral --json -- 简单介绍下自己"}
```

下面是 `sn-cli req api-cc "简单介绍下自己"` 对应的 API 日志示例。文件中仍是单行
JSONL，这里只为阅读展开；`response.data` 中的每个元素都是按到达顺序保存的原始
JSON response/frame（经过 secret 脱敏），不是 Runtime event：

```json
{
  "time": "2026-08-04 10:40:23",
  "namespace": "req",
  "profile": "api-cc",
  "source": "sn-cli req api-cc 简单介绍下自己",
  "call_id": "call_0123456789abcdef0123456789abcdef",
  "request": {
    "method": "POST",
    "url": "https://open.bigmodel.cn/api/anthropic/v1/messages",
    "headers": {
      "Accept": "text/event-stream, application/json",
      "Anthropic-Version": "2023-06-01",
      "Content-Type": "application/json",
      "X-Api-Key": "${Z_AI_API_KEY}"
    },
    "body": {
      "model": "glm-5.2",
      "messages": [
        {
          "role": "user",
          "content": [
            {"type": "text", "text": "简单介绍下自己"}
          ]
        }
      ],
      "max_tokens": 16384,
      "stream": true
    }
  },
  "response": {
    "status": 200,
    "headers": {
      "Content-Type": "text/event-stream"
    },
    "data": [
      {"type": "message_start", "message": {"id": "msg_example", "model": "glm-5.2", "usage": {"input_tokens": 12}}},
      {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
      {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "我是一个AI助手。"}},
      {"type": "message_delta", "delta": {"stop_reason": "end_turn"}, "usage": {"output_tokens": 9}},
      {"type": "message_stop"}
    ]
  },
  "error": null
}
```

API `request` 固定为 `method/url/headers/body`，不保存 curl 字符串；普通 JSON
response 作为 `data` 的单元素，SSE 收集全部有效 JSON `data:` frame，OpenAI
`[DONE]` 不保存。网络错误时 `response` 为 `null`，`error` 保存脱敏后的 typed
RuntimeError。API key 等 header 使用 Profile 的 `${VAR}` 引用（OpenAI 裸 key 会记成
`Bearer ${VAR}`），字面量敏感 header、cookie、URL query 及 response/error 中回显的
secret 记为 `[REDACTED]`。Driver 不自动跟随 redirect，3xx 作为这一次 Provider
response/error 记录，避免一次调用产生未分账的多个 HTTP request。

execution log 是 best-effort 诊断：写入错误、锁冲突、非法路径或 observer 异常都
直接丢弃，不改变请求结果、stream、exit code、retry、Session 或 Run。写入不与执行
建事务，也不做 `fsync`。当前没有 retention、总量限制或自动 GC；旧 flat `.jsonl`
保持原样，不迁移、不读取。

## 5. Profile 管理和执行入口

### 5.1 `profile list`

语法：

```text
sn-cli profile list
sn-cli --json profile list
```

用途：列出 active home 中所有 Profile 的 ID 和类型。

Human 示例：

```text
Profiles (2)
  api-cx  api
  cx      cli
```

适用场景：

- 确认安装后实际生效了哪些 Profile。
- 确认一个 Profile 是 CLI 还是 API。
- 排查 `unknown profile`。

### 5.2 `profile show`

语法：

```text
sn-cli profile show <profile-id>
sn-cli --json profile show <profile-id>
```

必须且只能提供一个 Profile ID。

示例：

```bash
sn-cli profile show cx
sn-cli --json profile show api-cx
```

适用场景：

- 查看实际 command、model、effort、prompt 和 cwd。
- 查看 API driver、endpoint、model、parameters 和 timeout。
- 调用前确认当前 active 配置，而不是只看源码 `configs/`。

### 5.3 `profile check`

语法：

```text
sn-cli profile check
sn-cli profile check <profile-id>
```

省略 ID 时检查全部 Profile。

示例：

```bash
sn-cli profile check
sn-cli profile check cx
```

它检查：

- JSON 结构和字段。
- `type=cli|api` 分流。
- CLI command adapter 是否已登记。
- args 是否符合 adapter grammar。
- model/effort/mode/output selector 是否冲突。
- API endpoint、headers 字段、parameters 和 timeout 结构。

它不检查：

- command 是否真实存在于 `PATH`。
- 环境变量是否已设置。
- cwd 是否真实存在。
- prompt 文件是否存在。
- API key 是否可用。
- Provider 网络是否可达。

需要本机依赖检查时使用 `sn-cli doctor`。

### 5.4 执行 namespace 与 Profile 类型

执行语义由入口固定，Profile `type` 做严格配对校验：

```text
sn-cli <cli-profile>       → interactive direct
sn-cli exec <cli-profile>  → non-interactive CLI exec
sn-cli req <api-profile>   → one API request
```

`profile` namespace 只保留 `list|show|check`，不能执行 Profile。bare 入口命中 API
Profile、`exec` 命中 API Profile、或 `req` 命中 CLI Profile，都会在启动进程或发出
网络请求前失败。

未知一级命令只按 CLI Profile ID 解析。例如：

```bash
sn-cli unknown-profile "回复OK"
```

如果 `configs/unknown-profile.json` 不存在，会返回：

```text
error: unknown profile "unknown-profile"
```

### 5.5 CLI direct 与 exec 参数

完整语法：

```text
sn-cli [--json] <cli-profile-id>
  [--model M|--model=M]
  [--effort E|--effort=E]
  [--prompt FILE_OR_TEXT|--prompt=FILE_OR_TEXT]
  [--cwd DIR|--cwd=DIR]
  [--]
  [INPUT]

sn-cli [--json] exec <cli-profile-id>
  [--model M|--model=M]
  [--effort E|--effort=E]
  [--prompt FILE_OR_TEXT|--prompt=FILE_OR_TEXT]
  [--cwd DIR|--cwd=DIR]
  [--]
  [INPUT]
```

| 参数 | 默认值 | 作用与限制 |
|---|---|---|
| `--model` | Profile `model` | 覆盖最终模型；最多一次 |
| `--effort` | Profile `effort` | 仅支持 `low/medium/high/xhigh/max`；最多一次 |
| `--prompt` | Profile `prompt` | 文件存在则读取，否则按文本；最多一次 |
| `--cwd` | Profile `cwd` 或调用 cwd | 覆盖工作目录；最多一次 |
| `INPUT` | 空 | 最多一个最终 quoted 参数 |
| stdin | 空 | stdin 非 TTY 时自动读取 |

所有 typed option 必须位于 positional `INPUT` 前。CLI Profile 不支持 raw/native
argv passthrough，未知 flag 会直接拒绝。

#### Prompt 合并顺序

```text
Profile prompt
→ CLI --prompt
→ 非 TTY stdin
→ positional INPUT
```

非空片段以换行连接，最终作为一个 `-- <prompt>` argv token 交给 command。

示例：

```bash
sn-cli exec cx \
  --prompt ./system-context.md \
  "补充要求"
```

如果文件内容是 `A`，最终位置参数是 `B`，目标程序收到的 prompt 是：

```text
A
B
```

路径不存在时，`--prompt` 被当作文本：

```bash
sn-cli exec cx --prompt "先只分析" "再给结论"
```

文件存在时必须是普通文件，不能是 symlink。

#### bare CLI direct

用途：打开原生交互 TUI。

```bash
sn-cli cx
sn-cli cx --effort high
sn-cli cx --model gpt-5.6-sol --effort=max
```

特点：

- 允许 prompt 为空。
- 使用 controlling TTY 作为 stdin。
- 没有 controlling TTY 时失败。
- 最终通过 process replacement 进入 Codex/Claude。

续接底层 CLI 既有会话（`resume` 子命令，仅 interactive direct）：

```bash
sn-cli cx resume                  # codex 交互 picker 选会话
sn-cli cx resume <codex-session-id>
sn-cli cc resume <claude-session-id>
```

- `<session-id>` 是底层 CLI（claude/codex）的**原生 session id**，由其自行存储；
  Runtime 不为 CLI direct 建立 Session/Run，只透传 + 翻译。
- claude→`--resume <id>`、codex→`resume <id>`；缺省 id 为 bare resume
  （claude 恢复最近会话、codex 进入交互 picker）。
- `resume` 后可跟 `--model`/`--effort`，但不再接受位置 input（继续输入用 `--prompt`）。
- `exec` 模式不支持 `resume`（resume 本质交互式）。

#### `exec` namespace

用途：执行一次任务并等待 CLI 退出。

```bash
sn-cli exec cx "回复OK"
sn-cli exec commit "为当前改动生成提交计划"
sn-cli exec cx-deep --effort high "分析当前仓库"
```

特点：

- 最终 prompt 必须非空。
- stdin 固定为 `/dev/null`。
- Codex adapter 自动插入 `exec`。
- Claude adapter 自动插入 `-p`。
- 保留目标进程的 stdout、stderr、exit code 和 signal。

`exec` namespace 只表示目标 CLI 的 non-interactive 执行模式，不表示后台运行，
也不决定使用当前终端还是 Tmux。当前调用会等待目标 CLI 退出；需要后台队列时使用
`session exec <profile> --queue`；需要带 canonical history 的长期窗口时使用
`session open`，只需要 Provider 原生 TUI 时使用 `tmux open`。

#### 动态 effort

```bash
sn-cli cx --effort low
sn-cli cx --effort medium
sn-cli cx --effort high
sn-cli cx --effort xhigh
sn-cli cx --effort max
```

映射：

```text
Codex  → -c model_reasoning_effort=<effort>
Claude → --effort <effort>
```

typed `--model`、`--effort` 优先于 Profile 字段和 `args` 中同类 selector。Adapter
会重建正确 argv 顺序，避免向目标 CLI 重复传入 `--model`。

#### cwd

```bash
sn-cli cx --cwd /Users/yang/mycode
sn-cli cx --cwd=../other-project
```

相对路径按调用 `sn-cli` 时的 cwd 解析。最终目录必须存在且可进入。

#### stdin 与 positional input

```bash
printf '%s' 'stdin内容' |
  sn-cli exec cx "位置参数内容"
```

最终 prompt：

```text
stdin内容
位置参数内容
```

### 5.6 `req` API 参数

完整语法：

```text
sn-cli [--json] req <api-profile-id>
  [--stream]
  [--request-file PATH|-]
  [--system TEXT]
  [--max-tokens POSITIVE_INT]
  [--temperature FINITE_0_TO_2]
  [--]
  [PROMPT]
```

| 参数 | 作用与限制 |
|---|---|
| `--stream` | 输出 NDJSON model events |
| `--request-file PATH` | 读取 canonical `ModelRequest` JSON |
| `--request-file -` | 从 stdin 读取整个 JSON request |
| `--system TEXT` | 设置 system prompt |
| `--max-tokens N` | Provider-neutral 输出上限，正整数；adapter 转为 wire 字段 |
| `--temperature T` | 有限值 `[0,2]` |
| `PROMPT` | 最终且最多一个位置参数；省略时从 stdin 读取 |

`req` 参数使用分离形式：

```bash
sn-cli req api-cx --temperature 0.2 "回复OK"
```

不要依赖：

```bash
sn-cli req api-cx --temperature=0.2 "回复OK"
```

两个 driver 使用同一个 token 参数：

```bash
# openai driver
sn-cli req api-cx --max-tokens 2048 "回复OK"

# anthropic driver
sn-cli req api-cc --max-tokens 2048 "回复OK"
```

`--request-file` 可以与 `--stream` 组合，但不能同时使用：

```text
positional prompt
--system
token limit
--temperature
```

`PATH` 文件必须是普通文件，不能是 symlink。JSON 使用 canonical `ModelRequest`
严格解码：输入最大 1 MiB，必须是有效 UTF-8、只包含一个完整 JSON document，
未知字段、重复 object key、trailing JSON/data 和超限输入都会被拒绝；canonical
text field 中的 NUL 由 `ModelRequest` validator 拒绝。
`-` 表示请求体完全来自 stdin，并使用相同校验。

`req` 只把已经读取的内容发送给 Provider。本地路径写在 prompt 中只是普通文本，
Provider 不能据此读取或写入本机文件；`--request-file` 也由 Runtime 先读取内容，
不会把路径交给模型。结果同步返回 stdout，不承诺产生本地文件副作用。

请求文件示例：

```json
{
  "system": "只输出结论",
  "messages": [
    {
      "role": "user",
      "content": "回复OK"
    }
  ],
  "options": {
    "max_output_tokens": 1024,
    "temperature": 0.2,
    "stop_sequences": ["END"]
  }
}
```

调用：

```bash
sn-cli req api-cx --request-file ./request.json
sn-cli req api-cx --stream --request-file ./request.json
cat request.json | sn-cli req api-cx --request-file -
```

普通 prompt 示例：

```bash
sn-cli req api-cc "回复OK"
sn-cli --json req api-cx "回复OK"
printf '%s' '回复OK' | sn-cli req api-cx
sn-cli req api-cx --system "只回答结论" "当前状态是什么"
```

当前 API Profile 不支持动态 `--model`。`--effort` 会先校验枚举，再明确返回：

```text
--effort is not supported for API profile "<id>"
```

一次 `req` 调用只执行一次 Provider call，不创建 Session 或 Run。模型返回
tool call 时输出 `requires_action`，但不会执行工具。

输出模式：

| 模式 | 输出 |
|---|---|
| 默认 Human | 有 assistant text 时直接打印文本 |
| Tool call | 打印 `requires_action` 和 tool call 摘要 |
| leading `--json` | 输出 `state=completed|requires_action` 和完整 result |
| `--stream` | NDJSON model events，最后输出 final envelope |

### 5.7 正式 release Profile

下表描述正式 release 的 Profile 清单。该清单由
`scripts/release-profile-files.sh` 唯一维护；release asset 只包含清单内文件。
`make install`、doctor 和 provider smoke 仍读取当前源码目录的全部
`configs/*.json`，因此可以保留不属于正式 release 的本地 Profile。安装后的实际
Profile 应使用 `profile list/show` 查询。

| ID | Adapter | Model | 配置重点 | 推荐入口 |
|---|---|---|---|---|
| `api-cc` | anthropic | `glm-5.2` | `max_tokens=16384`、timeout 50m | `req api-cc` |
| `api-cx` | openai | `qwen3.7-max` | `max_tokens=16384`、timeout 5m | `req api-cx` |
| `cc` | Claude CLI | `glm-5.2` | max | `cc` 或 `exec cc` |
| `cc-glm` | Claude CLI | `glm-5.2` | permission bypass | `exec cc-glm` |
| `cc-kmm` | Claude CLI | `claude-fable-5` | permission bypass | `cc-kmm` 或 `exec cc-kmm` |
| `cx` | Codex CLI | `gpt-5.6-sol` | xhigh | `cx` 或 `exec cx` |
| `commit` | Codex CLI | `gpt-5.3-codex-spark` | xhigh、read-only | `exec commit` |
| `cx-adv` | Codex CLI | `gpt-5.6-terra` | max、danger-full-access | `exec cx-adv` |
| `cx-deep` | Codex CLI | `gpt-5.6-sol` | max、search、danger-full-access | `exec cx-deep` |
| `cx-image` | Codex CLI | `gpt-5.6-sol` | xhigh | `exec cx-image` |
| `cx-spark` | Codex CLI | `gpt-5.3-codex-spark` | xhigh、read-only | `exec cx-spark` |

`cc-glm` 和 `cc-kmm` 的源码配置包含 permission bypass 选项；
`cx-adv`、`cx-deep` 允许 danger-full-access。使用这些 Profile 等于接受对应目标
CLI 的权限配置，调用前应通过 `profile show` 确认 active 配置。

## 6. Session

Session 是文件型 canonical 会话，持久化：

```text
Session
Turn
Message
Event
Execution
```

Session 的主要用途：

- 在多轮执行之间保存用户和 assistant message。
- 在不同 Turn 中切换 CLI/API Profile 或 Provider。
- 记录 CLI managed subprocess 的启动、退出和 reconciliation 事实。
- API 返回 tool call 时进入 `requires_action`，等待外部提交 tool result。
- 为 Agent Run 提供可选的消息投影目标。

Session 不自动执行 tool call。

### 6.1 `session exec` 与 `session req`

语法：

```text
sn-cli session exec <cli-profile-id>
  [--queue]
  [--session-id ID]
  [--task-id ID]
  [--retention ephemeral|standard|pinned]
  [--model M]
  [--effort E]
  [--cwd DIR]
  [INPUT]

sn-cli session req <api-profile-id>
  [--queue]
  [--session-id ID]
  [--task-id ID]
  [--retention ephemeral|standard|pinned]
  [--max-tokens N]
  [--temperature T]
  [INPUT]
```

用途：默认在当前进程中同步执行一个有记录的 Session Turn。`session exec` 只接受
CLI Profile；`session req` 只接受 API Profile。两条命令进入同一个
Provider-neutral Session service，底层 Session/Turn/Message/Execution schema 不变。

同步结果中的 `run_id` 是该 Session Turn 的执行关联 ID。同步执行不创建
SQLite Durable Run，因此不能默认用这个 ID 调用 `sn-cli run get`；应通过
`session show|messages|events|executions` 查询会话事实。只有带 `--queue` 的 Session
入口、`agent` 或 `run retry` 创建的 Durable Run ID 才进入 `sn-cli run ...` 控制面。

Profile ID 必须紧跟 `exec|req`，所有 option 位于 Profile ID 之后，input 最后：

```bash
sn-cli session exec cx --effort high "继续处理"
```

下面顺序不合法：

```bash
sn-cli session exec --effort high cx "继续处理"
```

公共参数：

| 参数 | 默认值 | 作用 |
|---|---|---|
| `--session-id` | 自动生成 | 复用指定 Session |
| `--task-id` | 空 | 关联外部任务 |
| `--retention` | `standard` | `ephemeral/standard/pinned` |
| `--queue` | false | 创建 Durable Run 并只入队 |
| `INPUT` | 无 | 最终输入，必须非空 |
| stdin | 空 | 非 TTY stdin 与 positional input 合并 |

CLI Profile 专用参数：

| 参数 | 作用 |
|---|---|
| `--model` | 覆盖 CLI Profile model |
| `--effort` | 覆盖 effort |
| `--cwd` | managed subprocess 工作目录 |

API Profile 专用参数：

| 参数 | 作用 |
|---|---|
| `--max-tokens` | Provider-neutral token limit；adapter 转为 wire 字段 |
| `--temperature` | `[0,2]` |

Session API 的公开 options 仅限本节语法和表格列出的参数，不暴露 system prompt。

`session exec` 固定使用 managed non-interactive mode：

- 创建受 Session 管理的 child process。
- 捕获 stdout、stderr 和 exit。
- Codex 使用 canonical JSONL。
- Claude 使用唯一的 canonical JSON result；Profile 不允许 `--verbose` 改写结果形态。
- 将 assistant text 投影为 Session Message。
- 记录 Execution identity 和终态。

`session req` 每个 Turn 仍然只做一次 Provider request；Session 负责保存历史和结果，
不会因此获得本地文件读写能力。需要 CLI Agent 按任务描述处理路径与产物时使用
`session exec`。

示例：

```bash
# 创建新 Session
sn-cli session exec cx "分析当前仓库"

# 复用 Session
sn-cli session exec cx \
  --session-id <session_id> \
  --effort high \
  "继续上一轮"

# API Session Turn
sn-cli session req api-cx \
  --max-tokens 4096 \
  --temperature 0.2 \
  "回复OK"

# stdin 和 positional input 合并
printf '%s' '补充上下文' |
  sn-cli session exec cx "执行任务"
```

### 6.2 `--queue`

`--queue` 位于 Profile ID 之后，其余参数与同步调用相同：

```text
sn-cli session exec <cli-profile-id> --queue [options] [INPUT]
sn-cli session req <api-profile-id> --queue [options] [INPUT]
```

区别：

- 不带 `--queue` 时立即同步执行。
- `--queue` 创建 Durable Session Run，初始状态为 `queued`。
- 入队本身不要求 server 已运行；从队列取出并执行需要 `sn-server` worker。
- `--queue` 不会自动启动 server。
- 相对 `--cwd` 在提交时按调用方 cwd 解析并固化。

两者的执行与 ID 边界：

| 对比项 | 默认同步 | `--queue` |
|---|---|---|
| 执行位置 | 当前 CLI 进程 | `sn-server` worker |
| 返回时机 | 当前 Turn 收口后 | Durable Run 进入 `queued` 后 |
| 持久化 | 文件型 Session | SQLite Durable Run；执行时写入文件型 Session |
| `run_id` | Session Turn 执行关联 ID | Durable Run ID，同时关联 Session Turn |
| `sn-cli run get|watch|result` | 不适用 | 支持 |

显式后台提交 Durable Session Run 由带 `--queue` 的 `session exec|req` 承担；
`session open` console 消费的每个 prompt 也会同步创建并执行一个 Durable Session Run；`run` namespace
不提供创建入口。它支持 `--retention`、API `--max-tokens/--temperature`，并沿用
Session 的 stdin 与 positional input 合并规则。

示例：

```bash
sn-cli server start

sn-cli session exec cx-deep \
  --queue \
  --retention pinned \
  "后台执行并记录"

sn-cli session exec cx-deep \
  --queue \
  --task-id background-analysis \
  --cwd "$PWD" \
  "执行后台任务"
```

返回的 `run_id` 可用于：

```bash
sn-cli run get --run-id <run_id>
sn-cli run watch --run-id <run_id>
sn-cli run result --run-id <run_id>
```

同一个 Session 同时只允许一个非终态 Durable Run。

### 6.3 Session input 合并

`session exec|req` 的 input 合并顺序：

```text
非 TTY stdin
→ positional INPUT
```

两段都存在时以换行连接。最终 input：

- 必须非空。
- 必须是 UTF-8。
- 不能包含 NUL。
- 最大 1 MiB。

### 6.4 `session list`

语法：

```text
sn-cli session list
sn-cli session list --state idle|active|blocked|archived
```

用途：查询全部 Session 或按状态过滤。

示例：

```bash
sn-cli session list
sn-cli session list --state blocked
sn-cli --json session list --state idle
```

### 6.5 `session show`

语法：

```text
sn-cli session show --session-id <session_id>
```

用途：查询一个 Session 的状态、retention、消息数和当前事实。

示例：

```bash
sn-cli session show --session-id <session_id>
sn-cli --json session show --session-id <session_id>
```

### 6.6 `session messages`

语法：

```text
sn-cli session messages
  --session-id <session_id>
  [--after-seq N]
```

`--after-seq` 默认 `0`，返回 `sequence > N` 的 Message。

适用场景：

- 获取完整对话历史。
- 增量同步新 Message。
- 检查 API/CLI/Agent 投影结果。

示例：

```bash
sn-cli session messages --session-id <session_id>
sn-cli session messages --session-id <session_id> --after-seq 10
```

### 6.7 `session events`

语法：

```text
sn-cli session events
  --session-id <session_id>
  [--after-seq N]
```

用途：查询 Session 生命周期和 Execution 事件。

示例：

```bash
sn-cli session events --session-id <session_id>
sn-cli --json session events \
  --session-id <session_id> \
  --after-seq 20
```

### 6.8 `session logs`

语法：

```text
sn-cli session logs
  --session-id <session_id>
  [--after-seq N]
  [--tail N]
```

默认 `--tail 120`。它读取的是 Session event view，不是未处理的 child
stdout/stderr 日志。

适用场景：

- 快速查看最近 Session 活动。
- 排查 Turn 卡在 `blocked` 或 Execution 未收口。

示例：

```bash
sn-cli session logs --session-id <session_id>
sn-cli session logs --session-id <session_id> --tail 50
```

### 6.9 `session executions`

语法：

```text
sn-cli session executions --session-id <session_id>
```

用途：列出 Session 的所有 CLI/API Execution。

示例：

```bash
sn-cli session executions --session-id <session_id>
sn-cli --json session executions --session-id <session_id>
```

### 6.10 `session execution`

语法：

```text
sn-cli session execution
  --session-id <session_id>
  --execution-id <execution_id>
```

用途：查看一个 Execution 的 lifecycle、identity、outcome 和错误。

示例：

```bash
sn-cli session execution \
  --session-id <session_id> \
  --execution-id <execution_id>
```

### 6.11 `session reconcile`

语法：

```text
sn-cli session reconcile
  --session-id <session_id>
  [--terminate | --acknowledge-unknown]
```

参数：

| 参数 | 作用 |
|---|---|
| 无额外 flag | 只在能证明 child 已消失时收口 |
| `--terminate` | 精确核对进程身份后 TERM，必要时 KILL |
| `--acknowledge-unknown` | 人工确认 API Execution 的结果未知 |

`--terminate` 与 `--acknowledge-unknown` 互斥。

适用场景：

- `sn-cli` 异常退出后 Session 仍为 `blocked`。
- managed CLI child 仍存活，需要受控终止。
- API request 的完成状态无法证明，需要显式记录 unknown。

示例：

```bash
sn-cli session reconcile --session-id <session_id>

sn-cli session reconcile \
  --session-id <session_id> \
  --terminate

sn-cli session reconcile \
  --session-id <session_id> \
  --acknowledge-unknown
```

`--terminate` 会核对 PID、process group、start token 和 executable identity，避免
PID reuse 导致终止错误进程。

Session 内部仍保留 `requires_action` 与 tool-result 投影，供领域测试和未来显式
extension 使用；stock `sn-cli` 和 HTTP API 不发布 tool-result 写入口。当前公开
Session request 也不声明 tools，因此不能把 Session 当成手工 tool loop。需要自动
执行 model/tool/tool-result 时使用 `agent <api-profile>`；需要后台执行时在 Profile
ID 后加入 `--queue`。

### 6.12 `session configure`

语法：

```text
sn-cli session configure
  --session-id <session_id>
  --retention ephemeral|standard|pinned
```

用途：修改 Session retention。

```bash
sn-cli session configure \
  --session-id <session_id> \
  --retention pinned
```

Retention：

| 值 | 适用场景 |
|---|---|
| `ephemeral` | 临时会话，可由 Session GC 回收 |
| `standard` | 默认普通会话 |
| `pinned` | 需要长期保留的会话 |

### 6.13 `session export`

语法：

```text
sn-cli session export
  --session-id <session_id>
  --output PATH
```

导出内容：

- Session
- Messages
- Events
- Executions

示例：

```bash
sn-cli session export \
  --session-id <session_id> \
  --output ./session-export.json
```

输出文件规则：

- 父目录必须已存在。
- 拒绝 symlink。
- 通过 `0600` 临时文件原子写入。
- 可覆盖已有普通文件。
- 当前不导出完整 Turn/context manifest。

### 6.14 `session delete`

语法：

```text
sn-cli session delete --session-id <session_id>
```

用途：删除不再使用的非活跃 Session。

规则：

- `active` 和 `blocked` Session 拒绝删除。
- 其他状态移动到 `sessions/_system/trash`。
- 这是可恢复移动，不是立即物理删除。
- 当前没有公开 `session restore` action。

示例：

```bash
sn-cli session delete --session-id <session_id>
```

### 6.15 `session gc`

语法：

```text
sn-cli session gc
  [--older-than-hours N]
  [--limit N]
  [--apply]
```

默认值：

| 参数 | 默认 |
|---|---:|
| `--older-than-hours` | `24` |
| `--limit` | `100`，最大 `1000` |
| `--apply` | false，默认 dry-run |

只选择：

```text
retention=ephemeral
state=idle
updated_at 早于 cutoff
```

预览：

```bash
sn-cli session gc
sn-cli session gc --older-than-hours 72 --limit 50
```

执行：

```bash
sn-cli session gc \
  --older-than-hours 72 \
  --limit 50 \
  --apply
```

Apply 后同样移动到 trash。

GC 的 JSON 结果包含：

- `candidates`：本轮扫描时符合条件的 Session；
- `moved`：`--apply` 后实际移入 trash 的 Session；
- `skipped`：扫描后已改为非 `ephemeral`、不再是 `idle`、`updated_at` 已刷新，
  或已被其他操作移动的 Session。没有跳过项时省略该字段。

`--apply` 会在每个 Session 的锁内重新检查 retention、state 和 cutoff。候选在扫描
后发生变化时会安全跳过，并继续处理本批其他候选。人类可读输出同样显示
`candidates`、`moved` 和 `skipped` 数量。

### 6.16 Session 状态

```text
Session:
idle | active | blocked | archived

Turn:
running | requires_action | completed | failed | cancelled

Execution lifecycle:
spawn_intent | running | settled

Execution outcome:
completed | failed | cancelled | unknown
```

典型变化：

```text
新 Session
  → idle

开始 Turn
  → Session active
  → Turn running
  → Execution spawn_intent/running

普通完成
  → Turn completed
  → Session idle
  → Execution settled/completed

API tool call
  → Turn requires_action
  → Session blocked

全部 tool result 已提交
  → Turn completed
  → Session idle

Agent paused
  → Turn requires_action
  → Session blocked
  → 只能通过对应 Run 的 run resume 恢复

Agent tool effect outcome unknown
  → Turn running
  → Session blocked，保留 active_turn_id
  → Execution settled/unknown
  → 只能通过对应 Run 的 run reconcile 收口
```

`archived` 当前有 schema、过滤和拒绝继续执行的语义，但没有公开 archive action。

### 6.17 Session 私有文件恢复协议

canonical Session fact 位于：

```text
sessions/<session_id>/
  session.json
  messages.jsonl
  events.jsonl
  turns/<turn_id>/turn.json
  turns/<turn_id>/context-manifest.json
  executions/<execution_id>.json
  context/current.json
```

Store 对现有目录逐组件使用 `O_NOFOLLOW` 打开并固定 directory device/inode；
canonical fact 和 private journal 必须是 single-link regular file。replace 使用
临时文件、`fsync`、identity-checked atomic publication；新 Session root 先在随机
临时目录写入 owner marker，再以 no-replace rename 发布。目录、文件或 lock 在
操作期间被替换时 fail closed。

一次 Session mutation 在同一 `flock` 内使用
`state/session-mutations/<session_id>.json`：

- private journal 固定 `mutation_version=3`，canonical Session 仍为
  `schema_version=2`；
- replace 目标保存完整 preimage；JSONL 目标保存原始 `size` 和
  `prefix_digest`；
- JSONL 的“追加”不是原地 append，而是读取既有内容、追加完整 JSON line 后
  atomic full-file rewrite；
- prepared recovery 只处理 journal 登记且 device/inode 属于该 mutation 的文件；
  JSONL 还必须满足当前长度不小于原始 `size`，且该前缀 digest 完全匹配，随后
  atomic rewrite 回原前缀；
- committed recovery 保留已发布 facts，只按 owner marker → journal 的顺序清理；
  identity、scope 或 durable state 无法证明时保留证据并报错，不启发式修复。

managed CLI 启动时在 `state/session-invocations/` 创建随机命名、mode `0600`、
single-link 的 manifest。Runtime 把 manifest directory/file identity 交给 private
helper；helper 通过 marker-before-exec handshake 后，按该 identity no-follow
读取、strict decode、删除 manifest，再 `exec` Provider。Service 初始化只清理
超过 24 小时、结构和 identity 均可证明属于 Runtime 的遗留 manifest；其他 entry
不猜测删除。

`session delete` 和 `session gc --apply` 在 rename 前持久化
`state/session-trash-moves/<session_id>.json`。该 journal 固定 `version=1`，绑定
source root device/inode，并只允许
`_system/trash/<timestamp>/<session_id>` target。恢复时：

- source 存在且 target 不存在：执行 no-replace rename，复核 target identity，
  再同步 target parent 和 `sessions/`；
- source 不存在且 target identity 匹配：视为 rename 已完成；
- source/target 同时存在、同时缺失或 identity 不匹配：保留 journal 并 fail
  closed；
- move 可证明完成后重建 `_system/index.json`，最后删除 journal。

以上恢复只处理文件事实，不调用 Provider、不执行 command/tool，也不与 SQLite
durable Run 建立伪 transaction。safe-fs 覆盖 symlink/hardlink、路径替换、
确定性并发 swap 和 crash；不承诺抵抗已获同 UID 任意代码执行、可持续枚举随机
quarantine 名称或使用 ptrace/kill 的攻击者。完整边界见
[SN Runtime 契约](docs/runtime-contract.md)。

### 6.18 Tmux-backed Session console

公开命令保持一个 action 层级：

```text
sn-cli session open <cli-profile-id>
  [--session-id <id>]
  [--retention ephemeral|standard|pinned]
  [--model M]
  [--effort low|medium|high|xhigh|max]
  [--cwd DIR]
  [INPUT]

sn-cli session send --session-id <id> [INPUT]
sn-cli session attach --session-id <id>
sn-cli session interrupt --session-id <id>
sn-cli session close --session-id <id>
sn-cli session close-all
```

`open` 只接受 CLI Profile。未给 `--session-id` 时创建一个 idle Session；给定时要求
该 Session 已存在且为 idle。Profile config、base prompt、effective model/effort/cwd
在 open 时冻结，每轮执行前复核；发生漂移时该轮 fail closed，关闭并重新 open 后才
能使用新配置。

控制台不是 Provider 原生 TUI。它是 tmux 中长期存活的 Runtime console：消费直接输入
或 `session send` 送达的 prompt 后，console 调用 `RunNow(kind=session)`，
先创建 SQLite Durable Run，再由
既有 non-interactive CLI Session executor 生成 Turn、Execution、user/assistant
Message 与 Event。Session projection 会携带已有 canonical history，因此不依赖
Codex/Claude TUI transcript 或 native resume。

示例：

```bash
sn-cli --json session open cx --cwd "$PWD" "分析当前仓库"
sn-cli session send --session-id <session_id> "继续下一步"
sn-cli session attach --session-id <session_id>

sn-cli session events --session-id <session_id>
sn-cli session messages --session-id <session_id>
sn-cli session close --session-id <session_id>
sn-cli session close-all
```

`open.initial_input_accepted=true` 和 `send.accepted=true` 只表示 tmux 已接收输入，
不表示 console 已消费，也不是 Run completion receipt；Run 创建后可从
`session events` 的 `run_id` 或
`run list/result` 读取 terminal fact。`interrupt` 先请求 canonical Run cancel。
`close` 会保留 Session；若 active Run 不能在 15 秒内 terminal，或进入
`needs_reconciliation`，它会保留 window 并返回 conflict，不能用 window exit 伪造
Turn 完成。

`close-all` 不接受参数。它对调用开始时所有 `binding.kind=session` 的 window 做
快照，逐个复用上述 close 流程；不触碰 raw Tmux window，也不删除 canonical Session。
空集合成功返回 `closed_count=0`。中途失败时已完成的 close 保留，剩余项可重跑。

## 7. Tmux

`sn-cli tmux` 是独立的原始 interactive process manager：

- 只接受 `type=cli` Profile。
- 固定使用 interactive adapter。
- 固定使用 `sn-session`；`tmux.server_mode=default|dedicated` 选择普通或专用
  Tmux server，缺省为 `default`。
- 每次 `open` 创建一个 managed window。
- 自身不创建 Runtime Session、Turn 或 Run；Session console 由 `session open` 组合。
- 不保存 pane transcript 或 paste 内容。
- 需要 `tmux >= 3.2`。

默认模式下普通 `tmux list-sessions` 可见 `sn-session`，也可用
`tmux attach-session -t sn-session` 进入并使用原生 window 导航；生命周期 mutation
仍应使用带 `tmux_id` 的 `sn-cli tmux ...` 命令。`dedicated` 模式保留隔离 socket。

Session console 与 raw Tmux window 统一使用 `open` action，但不能省略 action：
`sn-cli <cli-profile-id>` 已是 direct Profile 入口，省略会使路由语义冲突。旧
`tmux start` 不提供 alias。

### 7.1 `tmux open`

语法：

```text
sn-cli tmux open
  <cli-profile-id>
  [--model M|--model=M]
  [--effort E|--effort=E]
  [--prompt FILE_OR_TEXT|--prompt=FILE_OR_TEXT]
  [--cwd DIR|--cwd=DIR]
  [INPUT]
```

四个 typed option：

- 每个最多一次。
- 必须位于 Profile ID 后、INPUT 前。
- 支持分离和 `=` 形式。

Prompt 合并：

```text
Profile prompt
→ --prompt
→ 非 TTY stdin
→ positional INPUT
```

初始 prompt 可以为空。

示例：

```bash
sn-cli tmux open cx
sn-cli tmux open cx --effort high "继续处理"
sn-cli tmux open cx --model gpt-5.6-sol --cwd "$PWD"
sn-cli tmux open cx --prompt ./context.md "补充任务"
sn-cli tmux open cx -- "-开头的初始输入"
```

返回一个 `tmux_id`，后续所有操作都使用这个 Runtime ID，而不是自己拼 Tmux
window name。

### 7.2 `tmux list`

语法：

```text
sn-cli tmux list
```

不接受参数。当前模式的 managed `sn-session` 不存在时，成功返回空列表。

```bash
sn-cli tmux list
sn-cli --json tmux list
```

### 7.3 `tmux show`

语法：

```text
sn-cli tmux show --tmux-id <id>
sn-cli tmux show --tmux-id=<id>
```

用途：查看 Profile、window、pane、状态、exit code 和身份信息。

```bash
sn-cli tmux show --tmux-id <tmux_id>
```

### 7.4 `tmux send`

语法：

```text
sn-cli tmux send
  --tmux-id <id>
  [--]
  [INPUT]
```

`send` 的 `--tmux-id` 只支持分离形式。

输入来源：

```text
非 TTY stdin
→ positional INPUT
```

最终内容必须非空，最大 1 MiB。Runtime 使用 Tmux paste-buffer 粘贴内容，然后
单独发送 Enter。

示例：

```bash
sn-cli tmux send --tmux-id <tmux_id> "继续"

printf '%s' '补充上下文' |
  sn-cli tmux send --tmux-id <tmux_id> "执行"

sn-cli tmux send --tmux-id <tmux_id> -- "-开头的输入"
```

返回 accepted 只表示 Tmux 接受动作，不表示目标 TUI 已处理完成。

### 7.5 `tmux attach`

语法：

```text
sn-cli tmux attach --tmux-id <id>
sn-cli tmux attach --tmux-id=<id>
```

用途：进入 managed Tmux window。

要求：

- stdin/stdout 必须是 TTY。
- 不支持 `--json`。
- nested Tmux attach 受到当前 managed server 身份限制。

```bash
sn-cli tmux attach --tmux-id <tmux_id>
```

### 7.6 `tmux interrupt`

语法：

```text
sn-cli tmux interrupt --tmux-id <id>
sn-cli tmux interrupt --tmux-id=<id>
```

仅对 `running` window 发送 `C-c`。

```bash
sn-cli tmux interrupt --tmux-id <tmux_id>
```

accepted 不表示目标已经完成中断处理。

### 7.7 `tmux stop`

语法：

```text
sn-cli tmux stop --tmux-id <id>
sn-cli tmux stop --tmux-id=<id>
```

用途：按精确 window identity kill managed window。

```bash
sn-cli tmux stop --tmux-id <tmux_id>
```

最后一个 managed window 被停止时，`default` 只删除 `sn-session`，不影响普通
server 中的其他 session；`dedicated` 清理隔离 server/socket。对于 `orphaned`
window，只有能够证明身份时才会停止。

### 7.8 `tmux stop-all`

语法：

```text
sn-cli tmux stop-all
```

它对调用开始时所有无 binding 的 raw window 做快照并逐个精确 `stop`。只要快照中
存在 Session binding，就在任何 mutation 前整体返回 conflict，并提示先执行
`sn-cli session close-all`；Tmux namespace 不越过 Session owner 直接关闭绑定窗口。

空集合成功返回 `stopped_count=0`。并发新建 window 不属于当前快照；中途失败可能
留下尚未处理的 raw window，可安全重跑。

### 7.9 Tmux 状态

```text
starting | running | exited | orphaned
```

| 状态 | 含义 |
|---|---|
| `starting` | helper 已登记，尚未完成 target exec |
| `running` | 目标 executable 身份已确认 |
| `exited` | pane 已退出，window 仍可因 remain-on-exit 保留 |
| `orphaned` | provisional window 未形成完整 record |

## 8. Agent

Agent Kernel 的公开入口为：

```text
sn-cli agent <api-profile-id> [--queue] [options] [input]
```

它只接受 API Profile，负责自动执行：

```text
model
→ tool call
→ configured tool
→ tool result
→ model
→ ...
```

与 `req` 的区别：

| 能力 | `req` | Agent |
|---|---:|---:|
| 调用模型 | 一次 | 多轮 |
| 自动执行工具 | 否 | 是 |
| Durable Run | 否 | 是 |
| Session 投影 | 否 | 可选 |
| budget | 无 Agent budget | 支持 |
| checkpoint/event | 否 | 是 |

### 8.1 `agent`

完整语法：

```text
sn-cli [--json] agent <api-profile-id>
  [--queue]
  [--session-id <session-id>]
  [--task-id <task-id>]
  [--stream]
  [--max-rounds <1..128>]
  [--max-tool-calls <1..1024>]
  [--max-total-tokens <positive-int64>]
  [--max-wall-time <1s..24h>]
  [--label <key=value>]...
  [--]
  [INPUT]
```

参数：

| 参数 | 默认 | 作用与限制 |
|---|---|---|
| `<api-profile-id>` | 无 | 必填，且必须紧跟 `agent` |
| `--queue` | false | 只创建 queued Run，由 worker 执行 |
| `--session-id` | 空 | 可选；把 Agent 消息投影到 Session |
| `--task-id` | 空 | 关联外部任务 |
| `--stream` | false | 输出 NDJSON events |
| `--max-rounds` | runtime.json，默认 16 | 最大 model round，`1..128` |
| `--max-tool-calls` | runtime.json，默认 64 | 最大 tool call，`1..1024` |
| `--max-total-tokens` | runtime.json，默认 0 | 正整数；配置 0 表示不限 |
| `--max-wall-time` | runtime.json，默认 15m | `1s..24h` |
| `--label key=value` | 空 | 可重复，key 必须唯一 |
| `INPUT` | stdin | 最终 Agent 输入 |

除 `--label` 外，每个 option 最多一次。Agent 参数只支持分离形式，不支持
`--name=value`。`--queue` 与 `--stream` 互斥：queued 调用只返回提交 receipt，后续
通过 `run watch` 观察事件。

Label 限制：

- 最多 32 个。
- key 最大 64 bytes。
- value 最大 512 bytes。
- value 可以为空。
- 同一个 key 只能出现一次。

输入规则：

- 有 positional input 时不读取 stdin。
- 没有 positional input 时读取非 TTY stdin。
- positional input 与 stdin 不合并。
- 输入必须非空，最大 1 MiB。

不带 `--queue` 时，Agent 仍先创建 Durable Run，但由当前调用同步执行到 terminal；
带 `--queue` 时只创建 `queued` Run，由 `sn-server` worker 执行。两种模式都可通过
`sn-cli run ...` 查询和控制。

Agent 不支持：

```text
--model
--effort
--cwd
```

模型由 API Profile 决定。工具根默认来自执行进程 cwd 和 `runtime.json` 的
`workspace_roots`。

### 8.2 Agent 示例

普通执行：

```bash
sn-cli agent api-cx \
  "审查当前仓库并给出结论"
```

覆盖 budget：

```bash
sn-cli agent api-cx \
  --max-rounds 32 \
  --max-tool-calls 128 \
  --max-wall-time 20m \
  "完成这个任务"
```

增加 label：

```bash
sn-cli agent api-cx \
  --label task=review \
  --label source=cli \
  "审查改动"
```

投影到 Session：

```bash
sn-cli agent api-cx \
  --session-id <session_id> \
  "继续这个会话"
```

流式事件：

```bash
sn-cli agent api-cx \
  --stream \
  "执行并持续输出事件"
```

Machine 输出：

```bash
sn-cli --json agent api-cx \
  "回复OK"
```

stdin：

```bash
printf '%s' '回复OK' |
  sn-cli agent api-cx
```

### 8.3 Agent 工具

默认启用：

```text
read_file
list_directory
```

可以在 `runtime.json` 中显式开启：

```text
write_file
```

`workspace_roots` 非空时限制工具可访问的根目录；必须配置绝对路径。为空时使用执行
进程 cwd。

`read_file`、`list_directory` 和 `write_file` 在构建 tool registry 时固定
workspace root、cwd 和 root device/inode，执行时通过 pinned directory FD、
逐组件 `O_NOFOLLOW` 和 identity 复核抵抗 symlink 与确定性 path swap。read 只接受
single-link regular file，拒绝 FIFO、hardlink 和超限内容；write 使用
crypto-random private temp、file fsync、atomic rename 和 parent directory fsync。
该边界不抵抗已完全控制同 UID 进程、可 ptrace 或可直接操纵既有 fd 的攻击者。

builtin 集合只提供受控文件操作。workspace-root/path 检查不是 OS sandbox，不能
安全约束任意本机 subprocess，因此 Agent 不提供任意 subprocess 执行能力。

Agent 虽然由当前 CLI 同步等待，但开始执行前会创建 Durable Run、checkpoint 和
event。可在另一个终端查询：

```bash
sn-cli run list --kind agent
sn-cli run get --run-id <run_id>
sn-cli run events --run-id <run_id>
```

### 8.4 Agent 执行快照与配置漂移

`agent`（默认同步或 `--queue`）在创建 Run 前冻结 Store-private、
versioned、non-secret execution snapshot。它包含：

```text
execution_contract_version
model_execution_snapshot
tool_execution_snapshot
tool_execution_digest
session_request_digest       # 仅使用 --session-id 时存在
session_config_digest        # 仅使用 --session-id 时存在
config_digest
request_digest
```

model snapshot 保存完整 API Profile、Profile digest 和实际 Provider driver 的
semantic identity；tool snapshot 保存 enabled definitions、
implementation/version、canonical roots/cwd configuration。Run 的
`config_digest` 绑定 Agent、model/Provider 和 tool identity，`request_digest`
再绑定 immutable Agent request。绑定 Session 时，Session Turn/Execution 继续使用
独立的 profile-only digest；不要把它们与 Run combined digest 比较。

private snapshot 位于 SQLite `private_request_json`，不会通过 `run get/list/result`、
event、watch、Session export、日志或错误输出；公共 Run Record 只返回 digest。
private 表示不公开，不表示加密，其中仍包含 endpoint、model、non-secret literal
header、tool schema、workspace root/cwd identity 等元数据。

headers 只冻结 `${VAR}` 引用名（header 名 + 环境变量名），不保存 resolved secret
value。相同引用名下轮换 secret 不改变 snapshot；下一次 Provider call 使用执行进程
当时可见的值。修改父 shell 不会改变已经运行的 `sn-server` 环境，需要让实际执行进程
获得新值。

current snapshot 指执行进程已经加载的 Profile/Provider/tool，不表示每轮重新读取
磁盘文件。Runtime 在 fresh Run creation、Agent Retry 创建前，以及每个新的
Session/model/tool side effect 前比较完整 snapshot。Profile、endpoint、model、
driver semantic version、enabled tool、tool schema、workspace roots/cwd 或 tool
implementation version 变化都会 fail closed；Resume 已接受但尚未推进时发生 drift，
Run 保留原 active pause，不执行新的副作用。

已经 durable 的 completed/failed/started effect、pause closure、terminal
projection、cancel 和 reconcile 只依赖 frozen snapshot 与 durable journal，不要求
current Profile、Provider 或 tool 仍存在。`started` tool effect 仍表示 outcome
unknown，不能重放，只能按 `needs_reconciliation` 流程收口。

snapshot equality 是配置与执行语义漂移门禁，不是 binary attestation、数字签名或
OS sandbox；实现行为改变但未同步 bump semantic version 时，Runtime 无法据此检测。

## 9. Durable Run

Run 是 SQLite WAL durable control plane，负责：

- queued/running/terminal 状态。
- 持久化 request/result/error。
- 事件序列。
- checkpoint。
- pause/resume。
- cancel。
- retry。
- reconciliation。
- GC。

Run kind：

```text
agent
session
```

Run 不是 Session 或 Agent 的第二套执行引擎，而是两者共用的持久控制面。它把提交方、
执行任务的 worker 和查询/控制任务的调用方解耦，使提交进程退出后仍可排队、观察、
取消、恢复和重试：

```text
execution owner ──--queue──> SQLite Durable Run ──dequeue──> sn-server worker
                              │                         ├─ kind=session → Session executor
                              │                         └─ kind=agent   → Agent Kernel
                              └─ get/list/events/watch/cancel/retry <── observer
```

`kind=session` 执行一个有记录的 Session Turn；`kind=agent` 执行 API-only
model/tool loop。两种执行都复用同一组 Run 状态、event、result/error 和 terminal
barrier，而不是各自实现一套后台队列与取消协议。

### 9.1 Run 状态

```text
queued
  → running
    ├─→ paused
    │    └─→ queued
    ├─→ needs_reconciliation
    ├─→ completed
    ├─→ failed
    └─→ cancelled
```

终态：

```text
completed
failed
cancelled
```

Terminal transition 会把 result/error、terminal event、`run.settled` 和
`settled_sequence` 在同一 SQLite transaction 中提交。

### 9.2 创建入口

`run` namespace 不接受 fresh submission。新的执行意图必须由拥有执行语义的入口创建：

```bash
# queued CLI Session Turn
sn-cli session exec cx --queue --cwd "$PWD" "后台执行并记录Session"

# queued API Session Turn
sn-cli session req api-cx --queue --max-tokens 4096 "后台请求"

# Agent：默认同步执行 Durable Run
sn-cli agent api-cx "分析当前问题"

# queued Agent Run
sn-cli agent api-cx --queue --label task=analysis "分析当前问题"
```

三条创建路径都要求 Profile ID 紧跟 execution namespace/action，option 位于其后，
input 最后。`--queue` 只入队且不自动启动 `sn-server`；`run` 只接收创建后返回的
`run_id` 做查询、观察、取消、恢复、重试、reconciliation 和 GC；其中 `retry` 会从
已有终态 Run 创建一个关联的新 Run，但不接受新的 Profile/input。

### 9.3 `run get`

语法：

```text
sn-cli run get --run-id <run_id>
```

用途：获取完整 Run Record，包括 request、state、result、error、pause 和 sequence。

```bash
sn-cli run get --run-id <run_id>
sn-cli --json run get --run-id <run_id>
```

### 9.4 `run list`

语法：

```text
sn-cli run list
  [--state queued|running|paused|needs_reconciliation|completed|failed|cancelled]
  [--kind agent|session]
  [--limit N]
```

默认 `--limit 100`，最大 `1000`。结果按创建时间从新到旧。

示例：

```bash
sn-cli run list
sn-cli run list --state queued
sn-cli run list --kind session --limit 50
sn-cli --json run list --state failed --kind agent
```

### 9.5 `run result`

语法：

```text
sn-cli run result --run-id <run_id>
```

用途：读取当前 `result/error/state/settled_sequence`，不会等待 Run 进入终态。

```bash
sn-cli run result --run-id <run_id>
sn-cli --json run result --run-id <run_id>
```

Human 输出会尽量提取 Session 或 Agent 的 assistant text，然后打印最终 state。

### 9.6 `run events`

语法：

```text
sn-cli run events
  --run-id <run_id>
  [--after-seq N]
```

默认 `--after-seq 0`，返回 `sequence > N` 的持久化事件，单次最多 1000 条。

```bash
sn-cli run events --run-id <run_id>
sn-cli --json run events --run-id <run_id> --after-seq 10
```

### 9.7 `run watch`

语法：

```text
sn-cli run watch
  --run-id <run_id>
  [--after-seq N]
```

用途：以 NDJSON 追踪事件，直到 Run terminal barrier 已完整提交。

```bash
sn-cli run watch --run-id <run_id>
sn-cli run watch --run-id <run_id> --after-seq 10
```

`watch` 本身就是 machine stream，不要求额外使用 `--json`。

### 9.8 `run cancel`

语法：

```text
sn-cli run cancel --run-id <run_id>
```

状态语义：

| 当前状态 | 行为 |
|---|---|
| `queued` | 立即转 `cancelled` |
| `paused` | 立即转 `cancelled` |
| `running` | 写入 durable 取消请求；拥有该 Run 的 worker 轮询 SQLite、取消 execution context 并收口 |
| `needs_reconciliation` | 拒绝 |
| terminal | 幂等返回原记录 |

`queued`/`paused` 先在 SQLite 持久化 cancellation reservation 并移出 queue，再由
对应 executor finalizer 和 terminal barrier 收口为 `cancelled`。若进程在两步之间
退出，server startup reconciliation 会用专用 keyset scan 排空 reservation；它不
依赖普通 `run list` 的 limit。

```bash
sn-cli run cancel --run-id <run_id>
```

`sn-cli run cancel` 不会自动调用 server HTTP，但独立 CLI 写入的 durable flag 会被
`sn-server` 中拥有该 Run 的 worker 观察，因此不要求控制请求与 executor 位于同一
进程。ordinary terminal/reconciliation publish 不能越过 cancellation
reservation；只有 kind-specific cancellation finalizer 可以消费 reservation。
`POST /v1/runs/{run_id}:cancel` 使用相同语义。

### 9.9 `run resume`

语法：

```text
sn-cli run resume
  --run-id <run_id>
  (--input-json JSON | --input-file FILE)
```

`--input-json` 与 `--input-file` 严格互斥，且必须提供一个。只接受 `paused` Run。
输入必须是 strict resume envelope：root 为 non-null object，只包含非空
`pause_id` 和必填 `input`；拒绝未知字段、重复字段和 trailing data。`input` 自身
可为任意 JSON value（包括 `null`），并继续由 active pause 的 JSON Schema 校验。

直接 JSON：

```bash
sn-cli run resume \
  --run-id <run_id> \
  --input-json '{"pause_id":"pause_1","input":{"approved":true}}'
```

文件：

```bash
sn-cli run resume \
  --run-id <run_id> \
  --input-file ./resume.json
```

文件规则：

- 必须是普通文件。
- 拒绝 symlink。
- 最大 1 MiB。
- 内容必须是上述 strict resume envelope。

Store 在同一 transaction 内重校 exact active pause、采样 `accepted_at`、写 resume
journal、清理 pause/error/cancel flag 并重新进入 `queued`。并发恢复只允许一个
成功；任何校验失败或零行 CAS 都不会产生 mutation。公开 Run Record 不返回最新
resume input。

pause/resume 是 Kernel extension control plane。stock builtin tools 不产生
`paused`，所以 `server info` 不把 `resume` 作为 stock capability 发布；底层
`run resume` CLI、HTTP route、Store state 和 validator contract 继续保留，供注入
会返回 Pause 的自定义 ToolExecutor 使用。

### 9.10 `run retry`

语法：

```text
sn-cli run retry --run-id <run_id>
```

只接受 terminal Run。它创建新的 Run ID，并设置：

```text
retry_of=<old_run_id>
```

原 Run 不被修改。

Agent Retry byte-for-byte 保留原 Run 的 private execution snapshot，不根据当前
Profile 或 `runtime.json` re-freeze。创建新 Run 前会完整比较 current
model/Provider/tool/Agent/Session identity；任何 drift 都返回 conflict，且不产生
新的 Run 或 Provider/tool side effect。相同 `${VAR}` 引用名下的 secret value
轮换不参与该比较。

```bash
sn-cli run retry --run-id <run_id>
```

### 9.11 `run reconcile`

语法必须严格为：

```text
sn-cli run reconcile --run-id <run_id>
```

`run reconcile` 是 unknown outcome 的显式确认入口：

- Session Run 根据已经人工 reconciliation 的 Session Execution evidence 收口；
- Agent Run 不重放 Agent 或 tool，保留 checkpoint、Run/Session event 与
  `tool_effects` evidence，并以 `failed` 结案；
- Agent 绑定 Session 时，该命令先把
  `blocked + active Turn(running) + Execution(settled/unknown)` 的 Session
  projection 幂等收口，再提交 Run terminal barrier；
- 如果进程在 unknown projection 前退出，Execution 可能仍为 `running`；Turn
  上预先原子保存的 `agent_owned=true` marker 与精确 `run_id` correlation
  仍由本命令收口；
- reconciliation 前不能在该 Session 创建新 Turn，完成后 Session 恢复 `idle`；
- 对已经完成 reconciliation 的 Run 重复执行，会返回同一 terminal Run，不增加
  第二组 terminal event；普通 terminal Agent Run 会返回 conflict。

Agent 的 `paused` 不使用本命令，应使用 `run resume`。Agent 绑定 Session 的
unknown effect 也不能改用 `session reconcile`；stock Session 不发布
tool-result 写入口。

```bash
sn-cli run reconcile --run-id <run_id>
```

### 9.12 `run gc`

语法：

```text
sn-cli run gc
  [--older-than DURATION]
  [--limit N]
  [--apply]
```

默认值：

| 参数 | 默认 |
|---|---|
| `--older-than` | `runtime.json` 的 `run.settled_retention`，默认 168h |
| `--limit` | 100，最大 1000 |
| `--apply` | false，默认 dry-run |

`--older-than` 最小 `1h`。

预览：

```bash
sn-cli run gc
sn-cli run gc --older-than 720h --limit 1000
```

执行：

```bash
sn-cli run gc \
  --older-than 720h \
  --limit 1000 \
  --apply
```

只删除 cutoff 之前的 terminal Run。与 Session GC 不同，Run GC apply 会永久删除
Run 及其 event、checkpoint 和 effect，不移动到 trash。

## 10. Doctor 与 Server

`sn-server` 同时承载：

- Runtime HTTP API。
- Durable Run scheduler/worker。
- Server lifecycle。

默认地址：

```text
127.0.0.1:8080
```

通过 `HTTP_ADDR` 覆盖监听地址。

### 10.1 `server info`

语法：

```text
sn-cli server info
sn-cli --json server info
```

不接受其他参数。

用途：显示：

- Runtime version。
- Runtime Home。
- active Profile。
- Run database。
- configured HTTP address。
- namespace 和 capability。

`configured_address` 只是配置值，不证明 server 正在监听。
stock `run` capability 不包含 `resume`；这不表示底层 `run resume` CLI/API 被删除，
而是表示默认启用的 builtin/MCP tool 都没有可公开到达的 Pause producer。

```bash
sn-cli --json server info
```

### 10.2 `doctor`

语法：

```text
sn-cli doctor
sn-cli --json doctor
```

不接受其他参数。

它检查：

- 完整 Runtime services 是否能加载。
- Run SQLite store。
- builtin 与已启用的 Tool Catalog definitions。
- 每个 CLI Profile command 是否能按该 Profile 的 `cwd`、`env`、`${VAR}`
  展开和最终 `PATH` 规则解析。
- 每个 API Profile 的 headers `${VAR}` 引用的环境变量是否非空。
- 每个已启用 MCP tool 的 headers `${VAR}` 引用的环境变量是否非空。
- audit log sink 是否可写。
- `tmux` binary/version，以及当前 `server_mode` 下 `sn-session` 的 owner/identity；
  同时报告 `tmux_window_count`。

它不会向 Provider 或 MCP endpoint 发起真实请求。

```bash
sn-cli doctor
```

依赖缺失时返回非零，并列出：

```text
missing command profiles
invalid command profiles
missing auth environment
tmux error
audit log error
```

成功的 Machine 输出使用 `missing_command_binaries`、
`invalid_command_profiles`、`command_profile_errors` 和
`missing_auth_environment`、`tmux_error`、`audit_log_error`；因此“command 不存在”
和“cwd/env/ref 配置无效”不会混为一类。它还返回 `log_root`、`audit_log` 与
`runtime_home`、`tmux_window_count`。检查失败时仍返回单一 canonical error envelope，message 会列出
上述失败类别与具体名称。

### 10.3 `server start`

语法：

```text
sn-cli server start
sn-cli --json server start
```

不接受动态参数。

行为：

- 固定启动 `${SN_CLI_HOME}/bin/sn-server`。
- binary 必须是可执行普通文件。
- 使用 `setsid` 后台启动。
- stdout/stderr 追加到 server log。
- 写入 PID record 并持有 lease。
- 已运行时幂等返回同一 PID。
- 成功后不把 server 留作 `sn-cli` 的 child wait。

示例：

```bash
sn-cli --json server start
```

首次启动的 Machine 输出包含：

```json
{
  "running": true,
  "pid": 12345,
  "binary": "/path/to/runtime/bin/sn-server",
  "log": "/path/to/runtime/logs/sn-server.log",
  "configured_address": "127.0.0.1:8080"
}
```

如果 server 已经运行，幂等分支只保证返回：

```json
{
  "running": true,
  "pid": 12345,
  "configured_address": "127.0.0.1:8080"
}
```

该分支不返回 `binary` 和 `log`。`binary` 固定为
`<runtime-home>/bin/sn-server`，Runtime Home 可通过 `server info` 查询；log
路径通过 `server status` 查询。

第三方托管可以读取返回的正整数 PID。

### 10.4 `server status`

语法：

```text
sn-cli server status
sn-cli --json server status
```

不接受其他参数。

状态通过 lifecycle lock、lease、PID record 和 process identity 共同判断，避免
PID reuse。

```bash
sn-cli server status
```

Machine 输出包含：

```text
running
pid
pid_file
lease_file
log
```

### 10.5 `server stop`

语法：

```text
sn-cli server stop
sn-cli --json server stop
```

不接受其他参数。

行为：

- 已停止时幂等成功。
- 运行时发送 `SIGTERM`。
- 每 100ms 检查状态。
- 最多等待 10 秒。
- 不自动发送 `SIGKILL`。

```bash
sn-cli server stop
```

### 10.6 `server update`

合法形式：

```text
sn-cli server update
sn-cli server update --check
sn-cli server update --dry-run
sn-cli server update --dry-run --version VERSION
sn-cli server update --version VERSION
```

| 参数/形式 | 行为 |
|---|---|
| 无参数 | 检查 latest；有新版本时下载、校验、激活 |
| `--check` | 只检查，不安装 |
| `--dry-run` | 只生成 archive、URL、checksum 计划 |
| `--version VERSION` | 指定目标版本 |

互斥：

- `--check` 与 `--dry-run` 互斥。
- `--check` 与 `--version` 互斥。
- `--dry-run --version VERSION` 合法。

示例：

```bash
sn-cli server update --check
sn-cli --json server update --dry-run
sn-cli server update --dry-run --version v1.2.3
sn-cli server update --version v1.2.3
```

更新 repository：

```text
SN_CLI_REPOSITORY
```

未设置时默认：

```text
yy003x/runtime
```

### 10.7 `server upgrade-check`

语法：

```text
sn-cli server upgrade-check
sn-cli server upgrade-check --resources DIR
```

`--resources` 默认：

```text
<runtime-home>/resources
```

这里的 `DIR` 是 active/staged-home shape（同时含 `schema/`、`tmux.conf`、
`release.json`），不是新 archive payload 的 `resources/` 子目录；payload 需先按
source→active 映射构造 staged home。

用途：在 activation 前检查：

- release manifest。
- binary/resource contract。
- process/quiescence。
- Session schema。
- Run database schema/integrity。
- active state 是否适合升级。

它只执行 preflight，不 staging、不替换 binary。

```bash
sn-cli server upgrade-check
sn-cli server upgrade-check --resources /tmp/staged-home/resources
```

### 10.8 `server upgrade-activate`（内部）

该 action 不在公开 root help 中，供 installer/update coordinator 调用。

```text
sn-cli server upgrade-activate
  --payload DIR
  --target-home DIR
  [--command-link PATH]
  [--coordinator-pid POSITIVE_INT]
  [--overwrite-configs]
  [--local-source-install]
```

规则：

- `--payload` 必填。
- `--target-home` 必填。
- target canonical path 必须等于当前 `SN_CLI_HOME`。
- 每个 option 最多一次。
- `--coordinator-pid` 必须是正整数，并在 activation 中进一步核对父进程身份。
- `--command-link` 必须位于 Runtime Home 外，目标固定为
  `<target-home>/bin/sn-cli`。
- command link 在 activation mutation 前通过 durable owner sidecar、owner
  `flock` 和 no-clobber symlink 预留；失败时保留 exact owner/link，retry 仅在
  owner 内容、inode identity 与 target 均未变化时复用。
- `--local-source-install` 自动启用 configs 覆盖并先停止 server。
- `--local-source-install` 不能与显式 `--overwrite-configs` 同时使用。
- 成功后不自动重启 server。
- 在创建 target state/lock/stage 或停止 server 前，candidate 先验证 payload 的完整
  Profile 语义、required `release/runtime.json`、`resources/tools/`、
  `release/tmux.conf`、`release/release.json` 和 `resources/schema/` 下三个具有固定
  identity/root shape 的可编译 Schema；merged staged home 随后二次验证。required
  文件缺失、symlink 或检查期间被替换都会失败。

日常用户不应手工调用该 action。

## 11. 安装与本地源码更新

### 11.1 构建

```bash
make check
make build sn-cli-build
make V=1 sn-cli-build
```

`V=1` 显示安全转义后的真实 argv，并实时透传子命令输出。

### 11.2 本地源码覆盖安装

```bash
make install
```

该入口用于本地源码调试：

- 构建完整 candidate。
- 校验 binary、profiles、tools、runtime config 和 resources。
- 自动停止受管 `sn-server`。
- 按 source→active 映射，用源码 `configs/`、`resources/{schema,tools}/` 和
  `release/` 覆盖 active home。
- 丢弃现有 `sessions/`、`state/session-locks/`、
  `state/session-invocations/`、`state/session-mutations/`、
  `state/session-trash-moves/` 和 `state/runtime.db*`。
- 成功后不自动启动 server。
- 安装前必须先停止全部由 `sn-cli tmux` 管理的 live window；preflight 会拒绝
  覆盖仍由 managed Tmux 使用的 active home，不会自动关闭交互窗口。

安装后：

```bash
sn-cli --version
sn-cli profile list
sn-cli profile check
sn-cli doctor
```

需要 worker/HTTP 时再显式启动：

```bash
sn-cli --json server start
```

### 11.3 临时 Home 安装

```bash
runtime_home="$(mktemp -d)"
install_dir="$(mktemp -d)"

bash install.sh \
  --binary ./bin/sn-cli \
  --server ./bin/sn-server \
  --configs ./configs \
  --resources ./resources \
  --release ./release \
  --home "$runtime_home" \
  --install-dir "$install_dir"
```

适用场景：

- 验证 candidate。
- 不污染 active `~/.sn`。
- CI/release check。
- 对比不同配置。

## 12. HTTP API

HTTP API 由 `sn-server` 暴露，默认地址是 `http://127.0.0.1:8080`。

非 loopback 地址必须设置：

```text
SN_SERVER_TOKEN
```

客户端使用：

```text
Authorization: Bearer <token>
```

### 12.1 公共 HTTP 规则

所有 `POST` route 都要求严格 JSON body；没有业务字段的 control action 使用
空 object `{}`：

```text
Content-Type: application/json
```

所有 JSON body 都限制为 1 MiB、合法 UTF-8 和单一完整 JSON value，并拒绝重复
object key、trailing JSON/data 和空 body。其余 shape/null 规则按三类 decoder
区分：

- Session/Run/Agent 等固定 Runtime DTO：root 必须是 non-null object，拒绝未知
  字段和任意显式 `null`；无业务字段的 control action 还必须精确为 `{}`；
- `POST /v1/model/generate`：root 必须是 non-null object，strict decode 后再执行
  canonical `GenerateRequest.Validate`，字段/null 语义以 canonical model contract
  为准；
- `POST /v1/runs/{run_id}:resume`：body 必须是 strict
  `{"pause_id":"...","input":...}` envelope；只有 `input` 可为
  object、array、string、number、boolean 或 `null`，具体 shape/null 语义由对应
  pause 的 JSON Schema 决定。

NUL 不是 JSON decoder 对所有 string 的全局禁令；只有进入 canonical text field
时由对应 validator 拒绝。`Content-Type` 缺失或不是 `application/json` 时返回
`415`。

`GET` route 的输入来自 query/header，不要求 `Content-Type`。集合查询
`GET /v1/sessions` 和 `GET /v1/runs` 只接受下文声明的 query parameter；每个
parameter 最多出现一次，malformed encoding、未知参数、重复参数和显式空值均返回
`400/invalid_request`。只有完全省略参数才使用“全部”或默认 `limit`。

进入 Runtime handler 后，普通错误 envelope 是：

```json
{
  "error": {
    "code": "invalid_request",
    "phase": "request",
    "message": "具体错误",
    "retryable": false
  }
}
```

常见状态码：

| 状态码 | 语义 |
|---:|---|
| `200` | 查询、同步执行或 control action 成功 |
| `201` | Session 创建成功 |
| `202` | Durable Run 已入队 |
| `400` | DTO、query、ID、状态或字段值非法 |
| `401` | Provider 或 server Bearer 认证失败 |
| `403` | Provider 权限拒绝 |
| `404` | 合法 ID 指向的 Session、Execution 或 Run 不存在 |
| `409` | 当前状态不允许该 mutation |
| `413` | canonical context overflow |
| `415` | JSON route 的 `Content-Type` 错误 |
| `429` | Provider rate limit |
| `499` | request 已取消 |
| `502` | Provider protocol/response 无效 |
| `503` | Provider 或 Runtime service 暂不可用 |
| `504` | Provider timeout |

malformed resource ID 返回 `400/invalid_request`；合法 ID 指向不存在的
Session、Execution 或 Run 时返回 `404/not_found`，不会以空事件或空列表伪装为
存在；Store 故障返回 `500/internal`。server Bearer 认证失败也使用 canonical JSON
error envelope，返回 `401/authentication_failed`。未知 route 使用标准 HTTP
`404`。

#### Duration 的 JSON wire 表示

HTTP 没有统一使用一种 duration 表示，必须按字段发送：

| 字段 | JSON 表示 | 省略或 `0` | 有效范围/示例 |
|---|---|---|---|
| `budget.max_wall_time` | 整数纳秒，即 Go `time.Duration` | 继承 `runtime.json` Agent budget | `1s..24h`；`15m` 是 `900000000000` |
| Session GC `older_than_hours` | 整数小时 | 默认 `24` 小时 | `1..2562047`，换算后能放入 `time.Duration` |
| Run GC `older_than` | Go duration string | `runtime.json` 的 settled retention，默认 `168h` | 至少 `1h`，例如 `"168h"` |

Run Record 中的 `request.agent_budget.max_wall_time` 也以整数纳秒返回。`time.Time`
字段由 Go JSON codec 输出 RFC3339/RFC3339Nano string。

### 12.2 Model

```text
POST /v1/model/generate
```

请求 DTO 是 canonical `GenerateRequest`：

```json
{
  "model_profile": "api-cx",
  "input": {
    "system": "可选 system prompt",
    "messages": [
      {"role": "user", "content": "回复OK"}
    ],
    "tools": [
      {
        "name": "lookup",
        "description": "可选说明",
        "input_schema": {
          "type": "object",
          "properties": {"query": {"type": "string"}},
          "required": ["query"],
          "additionalProperties": false
        }
      }
    ],
    "options": {
      "max_output_tokens": 4096,
      "temperature": 0.2,
      "stop_sequences": ["END"]
    },
    "trace": {
      "labels": {"task_id": "demo"}
    }
  }
}
```

| 字段 | 必填 | 说明 |
|---|---:|---|
| `model_profile` | 是 | active `type=api` Profile ID |
| `input.messages` | 是 | 非空 canonical Message 数组 |
| `input.system` | 否 | Provider-neutral system text |
| `input.tools` | 否 | canonical tool definitions；Runtime 只传给 Provider |
| `input.options.max_output_tokens` | 否 | 正整数 |
| `input.options.temperature` | 否 | 有限数，范围 `[0,2]` |
| `input.options.top_p` | 否 | 有限数，范围 `[0,1]` |
| `input.options.stop_sequences` | 否 | 最多 4 个非空字符串，每项最多 1024 bytes |
| `input.trace.labels` | 否 | 最多 32 个 label |

canonical tool Message 使用 `role=tool`、`tool_call_id`、`content`，并可用
`is_error=true` 表示工具失败。`is_error` 只允许出现在 tool Message；Runtime 会在
后续 Provider 请求中保留该语义。

普通请求成功返回 `200` 和 canonical `ModelResult`：

```json
{
  "message": {"role": "assistant", "content": "OK"},
  "finish_reason": "stop",
  "usage": {},
  "provider": {}
}
```

增加 `Accept: text/event-stream` 时返回 SSE canonical Event；每帧包含 `id`、
`event` 和 JSON `data`。Provider 失败发生在 SSE 已开始后时发送
`event: error`，其 `data` 是 `RuntimeError`。

### 12.3 Session routes

```text
GET  /v1/sessions
POST /v1/sessions
POST /v1/sessions/gc

GET  /v1/sessions/{session_id}
POST /v1/sessions/{session_id}:reconcile

GET  /v1/sessions/{session_id}/messages
GET  /v1/sessions/{session_id}/events
GET  /v1/sessions/{session_id}/executions
GET  /v1/sessions/{session_id}/executions/{execution_id}
GET  /v1/sessions/{session_id}/watch

POST /v1/sessions/{session_id}/turns
```

#### Session 查询、创建与 GC

| Route | 输入 | 默认 | 成功响应 |
|---|---|---|---|
| `GET /v1/sessions` | query `state=idle|active|blocked|archived` | 省略时表示全部 | `200 {"sessions":[Session...]}` |
| `POST /v1/sessions` | `{"retention":"ephemeral|standard|pinned"}` | `standard` | `201 Session` |
| `POST /v1/sessions/gc` | `{"older_than_hours":N,"limit":N,"apply":bool}` | 字段省略时为 `24`、`100`、`false` | `200 GCResult` |
| `GET /v1/sessions/{session_id}` | path ID | 无 | `200 Session` |

Session GC 的 `older_than_hours` 必须在 `1..2562047`，
`limit` 范围为 `1..1000`；显式 `0` 非法，只有省略字段才使用默认值。
`apply=false` 只返回 candidate；`apply=true` 移动仍满足条件的 Session，并返回
`moved`/`skipped`。

Message、Event 和 Execution 查询：

| Route | Query | 成功响应 |
|---|---|---|
| `GET /v1/sessions/{id}/messages` | `after_seq`，默认 `0` | `200 {"messages":[MessageRecord...]}` |
| `GET /v1/sessions/{id}/events` | `after_seq`，默认 `0` | `200 {"events":[EventRecord...]}` |
| `GET /v1/sessions/{id}/executions` | 无 | `200 {"executions":[Execution...]}` |
| `GET /v1/sessions/{id}/executions/{execution_id}` | 无 | `200 Execution` |

`after_seq=N` 只返回 `sequence > N` 的记录，必须是 `uint64`。

#### 创建 Session Turn

```text
POST /v1/sessions/{session_id}/turns
```

请求：

```json
{
  "profile_id": "api-cx",
  "input": "继续分析",
  "task_id": "可选外部任务ID",
  "model": "",
  "effort": "",
  "cwd": "",
  "model_options": {
    "max_output_tokens": 4096,
    "temperature": 0.2,
    "stop_sequences": ["END"]
  }
}
```

`profile_id` 和非空 `input` 必填。CLI Profile 可使用 `model`、`effort`、`cwd`，
但拒绝 `model_options`；API Profile 可使用 `model_options`，但拒绝 `model`、
`effort`、`cwd`。成功同步执行并返回 `200 Session RunResult`。

#### Session reconciliation

```text
POST /v1/sessions/{session_id}:reconcile
```

请求：

```json
{"terminate": false, "acknowledge_unknown": false}
```

两项不能同时为 `true`。成功返回 `200 ReconcileResult`。

#### Session watch

```text
GET /v1/sessions/{session_id}/watch
Accept: text/event-stream
```

持续输出 Session EventRecord SSE，直到客户端断开。支持 query `after_seq` 和 header
`Last-Event-ID`；非零 `after_seq` 优先，否则使用 `Last-Event-ID`，两者都省略时从
`0` 开始。

### 12.4 Agent route

```text
POST /v1/agent/run
```

请求：

```json
{
  "profile_id": "api-cx",
  "input": "完成任务",
  "session_id": "可选 Session ID",
  "task_id": "可选外部任务 ID",
  "labels": {"source": "http"},
  "budget": {
    "max_rounds": 16,
    "max_tool_calls": 64,
    "max_total_tokens": 0,
    "max_wall_time": 900000000000
  }
}
```

| 字段 | 必填 | 说明 |
|---|---:|---|
| `profile_id` | 是 | 必须是 active API Profile |
| `input` | 是 | 非空 Agent 输入 |
| `session_id` | 否 | 把 Agent projection 写入已有 Session |
| `task_id` | 否 | 外部 correlation ID |
| `labels` | 否 | 最多 32 个，限制与 CLI 相同 |
| `budget` | 否 | 每个 `0`/省略字段继承 server 启动时的 `runtime.json` |

`max_rounds` 范围 `1..128`，`max_tool_calls` 范围 `1..1024`，
`max_total_tokens` 非负；`max_wall_time` 使用整数纳秒，范围 `1s..24h`。

普通 JSON 请求同步执行并返回 `200 Run Record`。请求包含：

```text
Accept: text/event-stream
```

时输出 canonical Event SSE。客户端断开后，Durable Run 继续执行。

HTTP Agent 使用与 [`Agent 执行快照与配置漂移`](#84-agent-执行快照与配置漂移)
相同的冻结、digest、drift 和 secret 语义。响应、SSE、日志和错误均不包含 private
snapshot。

### 12.5 Run routes

```text
GET  /v1/runs
POST /v1/runs
POST /v1/runs/gc

GET  /v1/runs/{run_id}
GET  /v1/runs/{run_id}/events

POST /v1/runs/{run_id}:cancel
POST /v1/runs/{run_id}:resume
POST /v1/runs/{run_id}:reconcile
```

#### Run 入队与查询

```text
POST /v1/runs
```

请求：

```json
{
  "kind": "agent",
  "profile_id": "api-cx",
  "input": "后台执行",
  "session_id": "",
  "task_id": "",
  "model": "",
  "effort": "",
  "model_options": {},
  "labels": {},
  "budget": {}
}
```

`kind`、`profile_id` 和非空 `input` 必填。`kind=agent|session` 的字段约束与 CLI
对应带 `--queue` 的 `agent` 或 `session exec|req` 相同；缺少 Session kind 的
`session_id` 时由 server 生成。Agent `budget` 使用与 `/v1/agent/run` 相同的默认值和纳秒表示。成功返回
`202 Run Record`，只表示入队，不表示已经执行。

`cwd` 只允许 `kind=session`；`kind=agent` 携带 `cwd` 返回
`invalid_request`。Session kind 使用 CLI Profile 时仍可按其契约提供 `cwd`。

`kind=agent` 在返回 `202` 前已经冻结并自校验 private execution snapshot；公共
Record 只返回 `request_digest/config_digest`，不会返回 private payload。

查询：

| Route | Query | 默认 | 成功响应 |
|---|---|---|---|
| `GET /v1/runs` | `state`、`kind=agent|session`、`limit=1..1000` | 全部 state/kind，`limit=100` | `200 {"runs":[Run Record...]}` |
| `GET /v1/runs/{run_id}` | 无 | 无 | `200 Run Record` |
| `GET /v1/runs/{run_id}/events` | `after_seq` | `0` | `200 {"events":[Event...]}`，单次最多 1000 条 |

`state` 可为 `queued|running|paused|needs_reconciliation|completed|failed|cancelled`。

#### Run event watch

```text
GET /v1/runs/{run_id}/events
Accept: text/event-stream
```

有 `Accept: text/event-stream` 时持续 watch，支持 `after_seq` 和
`Last-Event-ID`；非零 `after_seq` 优先。Run terminal barrier 发布完成后结束。

#### Run control

| Route | Request body | 成功响应 |
|---|---|---|
| `POST /v1/runs/{id}:cancel` | 严格空 object `{}` | `200 Run Record` |
| `POST /v1/runs/{id}:resume` | strict `pause_id` + `input` envelope | `200 Run Record` |
| `POST /v1/runs/{id}:reconcile` | `{}` | `200 Run Record` |

状态语义分别见 [`run cancel`](#98-run-cancel)、[`run resume`](#99-run-resume) 和
[`run reconcile`](#910-run-reconcile)。

#### Run GC

```text
POST /v1/runs/gc
```

请求：

```json
{"older_than": "168h", "limit": 100, "apply": false}
```

`older_than` 是至少 `1h` 的 Go duration string；省略时使用 `runtime.json` 的
`run.settled_retention`，默认 `168h`。`limit` 范围为 `1..1000`；显式 `0`
非法，省略时使用 service 默认 `100`。`apply=false` 返回 candidate，
`apply=true` 永久删除；成功返回 `200 Run GCResult`。

### 12.6 当前没有的 HTTP route

HTTP 当前没有：

- Run retry route。
- 独立 Run result route；使用 `GET /v1/runs/{id}`。
- Tmux route。
- Profile 配置管理 route。
- Session export/delete/configure route。

## 13. 常见完整工作流

### 13.1 临时切换 effort

```bash
sn-cli cx --effort high
```

或者 one-shot：

```bash
sn-cli exec cx --effort max "深度分析当前问题"
```

不需要修改 `configs/cx.json`。

### 13.2 Prompt 文件加临时补充

```bash
sn-cli exec cx \
  --prompt ./base-prompt.md \
  "只输出最终结论"
```

最终顺序是文件内容在前，临时文本在后。

### 13.3 创建并复用 Session

```bash
sn-cli --json session req api-cx "第一轮"
```

从返回结果取得 `session_id`：

```bash
sn-cli session req api-cc \
  --session-id <session_id> \
  "第二轮，切换Provider"
```

查看历史：

```bash
sn-cli session messages --session-id <session_id>
```

### 13.4 后台 Session

```bash
sn-cli server start

sn-cli --json session exec cx-deep \
  --queue \
  --retention pinned \
  "后台执行"
```

取得 `run_id` 后：

```bash
sn-cli run watch --run-id <run_id>
sn-cli run result --run-id <run_id>
```

### 13.5 长期交互窗口

需要 canonical history、durable Run 和多轮输入时：

```bash
sn-cli --json session open cx --cwd "$PWD" "打开长期任务"
sn-cli session send --session-id <session_id> "继续"
sn-cli session attach --session-id <session_id>
sn-cli session interrupt --session-id <session_id>
sn-cli session close --session-id <session_id>
```

只需要 Provider 原生 TUI、不需要 Session/Run 时才使用原始 Tmux：

```bash
sn-cli --json tmux open cx "打开长期任务"
```

后续：

```bash
sn-cli tmux send --tmux-id <tmux_id> "继续"
sn-cli tmux attach --tmux-id <tmux_id>
sn-cli tmux interrupt --tmux-id <tmux_id>
sn-cli tmux stop --tmux-id <tmux_id>
```

这套原始 Tmux 流程没有 Session history。

### 13.6 Agent 自动工具循环

```bash
sn-cli agent api-cx \
  --max-wall-time 20m \
  --label task=repository-review \
  "检查仓库并输出结论"
```

需要把结果投影到 Session：

```bash
sn-cli agent api-cx \
  --session-id <session_id> \
  "继续检查"
```

### 13.7 Durable 后台 Agent

```bash
sn-cli server start

sn-cli --json agent api-cx \
  --queue \
  --label task=analysis \
  "分析当前问题"
```

查询：

```bash
sn-cli run list --kind agent
sn-cli run get --run-id <run_id>
sn-cli run events --run-id <run_id>
sn-cli run watch --run-id <run_id>
```

### 13.8 Machine 集成

Shell 脚本应使用 leading `--json`：

```bash
result="$(
  sn-cli --json server start
)"
```

不要解析 Human 文本，也不要直接读取 `${SN_CLI_HOME}` 内部文件。

流式入口按 NDJSON 逐行处理：

```bash
sn-cli run watch --run-id <run_id> |
  while IFS= read -r event; do
    jq -c . <<<"$event"
  done
```

## 14. 常见错误与排查

### `error: direct requires a CLI profile; "api-cc" is an API profile`

bare Profile 只接受 CLI Profile；API Profile 必须通过 `req` namespace 调用：

```bash
sn-cli req api-cc "回复OK"
```

如果已经使用 `req` 仍失败，再检查 binary、active Profile 与受管 resources：

检查：

```bash
command -v sn-cli
sn-cli --version
sn-cli profile list
```

必要时在源码根目录执行：

```bash
make install
```

### `error: unknown profile "..."`

检查 active config：

```bash
sn-cli profile list
ls "${SN_CLI_HOME:-$HOME/.sn}/configs"
```

Profile 来自 active home，不是自动读取当前仓库 `configs/`。

### `--json` 被报告为未知参数

确保它位于第一个参数：

```bash
sn-cli --json run list
```

### `--model ... cannot be used multiple times`

不要把模型 selector 同时写入未经 adapter 管理的多个位置。当前协议应把动态模型放在
typed 参数：

```bash
sn-cli cx --model gpt-5.6-sol "回复OK"
```

Profile 配置使用顶层 `model`，不要在 `args` 中再重复同类 selector。使用：

```bash
sn-cli profile check cx
```

检查 adapter plan。

### `exec prompt is required`

`sn-cli exec` 必须有最终 prompt。可以通过：

```text
Profile prompt
--prompt
stdin
positional INPUT
```

任一来源提供。

### Session 参数被报告 unknown

Session Profile ID 必须紧跟 `exec|req`，option 放在 Profile ID 后：

```bash
sn-cli session exec cx --effort high "执行"
```

不能写：

```bash
sn-cli session exec --effort high cx "执行"
```

### `--queue` 创建的 Run 一直 queued

检查 server：

```bash
sn-cli server status
sn-cli server start
sn-cli doctor
```

入队不会自动启动 worker。

### Tmux 启动失败

检查：

```bash
tmux -V
sn-cli profile show <profile-id>
sn-cli profile check <profile-id>
```

`tmux open`：

- 需要 `tmux >= 3.2`。
- 只接受 CLI Profile。

### API Profile 认证失败

查看 Profile 使用的环境变量名：

```bash
sn-cli --json profile show <api-profile-id>
```

运行本机依赖检查：

```bash
sn-cli doctor
```

不要把 secret 写入 Profile 或命令历史。

## 15. 内部入口和协议边界

### 内部入口

以下入口属于 Runtime 自身，不是日常用户 API：

```text
sn-cli server upgrade-activate ...
sn-cli __sn_tmux_helper --manifest ABS_PATH
sn-cli __sn_session_terminal_helper ...
```

`__sn_tmux_helper` 由 Tmux Service 写入 manifest 后在 pane 中调用；
`__sn_session_terminal_helper` 只由 `session open` 作为 cooperative target 调用。
两者都是 private protocol，不要手工构造。

### 入口职责

```text
直接交互 TUI  → <cli-profile>
直接一次执行  → exec <cli-profile>
一次 API 请求 → req <api-profile>
有记录的执行  → session exec|req <profile> [--queue]
长期 Session   → session open|send|attach|interrupt|close|close-all
原生 TUI 窗口 → tmux open|send|attach|interrupt|stop|stop-all
自动工具循环  → agent <api-profile> [--queue]
已有 Run 控制 → run ...
```

## 16. 契约文档

[SN Runtime 契约](docs/runtime-contract.md) 是当前架构、配置、CLI/HTTP、Session、
Run、Agent、Tmux 与激活语义的唯一契约文档。
