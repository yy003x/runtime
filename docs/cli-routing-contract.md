# CLI 路由契约

## 根路由

路由优先级固定为：

1. 可选且只允许位于 argv 第一项的 global `--json`；
2. `-h|--help|help` 与 `--version|version`；
3. 固定 namespace：`profile|session|tmux|agent|run|server`；
4. active `commands/<id>.json` 声明的 CLI Profile shortcut；
5. 未命中即失败。

namespace 或 shortcut 后的 `--json` 不属于 root。例如 `sn-cli cx --json` 会把
`--json` 原样传给 Codex。固定 namespace 和 `profile list|show|check` 保留 ID
不能被配置覆盖。

## Profile

```text
sn-cli profile <id> [--model M] [--effort E] [--prompt FILE_OR_TEXT]
                    [--exec|--exec=true|--exec=false] [--cwd DIR] [input]
sn-cli profile list
sn-cli profile show <id>
sn-cli profile check [id]
```

不存在 `profile exec|open` action。CLI Profile 在 adapter 校验后 process
replacement；API Profile 做一次 API call。二者都不创建 Session 或 durable Run。

CLI Profile prompt 按 Profile `prompt`、`--prompt`、piped stdin、位置 input 合并。
effective `exec=true` 时 prompt 必须非空，`exec=false` 时允许空 prompt。Runtime
不把 CLI Profile 的原生 stdout/stderr/exit 包装成 JSON。

顶层 shortcut 使用 Profile `exec`，但忽略 Profile prompt 和 Runtime typed
parser；调用方 native args/stdin 原样追加。它是唯一硬兼容面：

```text
sn-cli cx [native-codex-args...]
sn-cli cc [native-claude-args...]
sn-cli cx-* [native-codex-args...]
```

## Session

```text
sn-cli session run [options] <profile-id> <input>
sn-cli session submit [options] <profile-id> <input>
sn-cli session list|show|messages|events|logs
sn-cli session executions --session-id <id>
sn-cli session execution --session-id <id> --execution-id <id>
sn-cli session reconcile --session-id <id>
                         [--terminate|--acknowledge-unknown]
sn-cli session tool-result
sn-cli session configure|export|delete|gc
```

Session 是记录 canonical Turn 的独立领域。API Profile 使用 API executor；CLI
Profile 固定 adapter `exec=true`，由 managed subprocess 捕获 stdout、stderr 和
exit，并解码稳定机器输出。Session 不读取 Profile `exec`，也不使用 Tmux。

CLI Session Turn override 只支持 `--model`、`--effort`、`--cwd`。API request
options 只适用于 API Profile，CLI override 只适用于 CLI Profile。输入来自 piped
stdin 和最后一个位置参数，合并后必须非空。

`run` 同步等待 terminal result；`submit` 创建 durable Run，并由 worker 执行同一
Session service。进程已经可能执行但结果未知时，Session 和 Run 进入显式
reconciliation，不自动重放。

## Tmux

```text
sn-cli tmux start [--model M] [--effort E] [--prompt FILE_OR_TEXT]
                  [--cwd DIR] <profile-id> [input]
sn-cli tmux list
sn-cli tmux show --tmux-id <id>
sn-cli tmux send --tmux-id <id> <input>
sn-cli tmux attach --tmux-id <id>
sn-cli tmux interrupt --tmux-id <id>
sn-cli tmux stop --tmux-id <id>
```

Tmux 是不创建 Runtime Session 的 interactive process manager。`start` 对任意合法
CLI Profile 固定 adapter `exec=false`，每次在专用 server 的 `sn-session` 中新增
window；初始 prompt 是最终 argv token，只有后续 `send` 使用安全 paste。
`attach` 是 human-only，要求 stdin/stdout TTY。

## Agent 与 Run

```text
sn-cli agent run --profile <api-profile-id> [options] <input>

sn-cli run submit --kind agent|session --profile <id> <input>
sn-cli run get|list|result|events|watch
sn-cli run cancel|resume|retry|reconcile
sn-cli run gc [--older-than 168h] [--limit 100] [--apply]
```

`agent run` 是唯一 API-only model/tool loop。`run submit` 是 durable queue 控制面。

## 输出

- Runtime 管理 action 默认输出 compact human 文本；
- leading global `--json` 选择 `schema_version=1`、`contract_version=3` machine
  contract；
- 非流失败 stdout 为空，stderr 只有一个 compact v3 error document；
- stream/watch 输出 NDJSON，成功时最后一行是唯一 final record；失败不输出 final；
- CLI Profile 和 shortcut 始终继承目标进程 stdout/stderr/exit；
- `tmux attach` 不支持 machine mode；
- `tmux list` 在专用 server 不存在时成功返回空集合。
