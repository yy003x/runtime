# SN Runtime 配置

## Source 与 active home

```text
source/payload configs/*.json              → ${SN_CLI_HOME}/configs/*.json
source/payload resources/tools/*.json      → ${SN_CLI_HOME}/tools/*.json
source/payload release/runtime.json        → ${SN_CLI_HOME}/runtime.json
source/payload resources/schema/*.json     → ${SN_CLI_HOME}/resources/schema/*.json
source/payload release/tmux.conf           → ${SN_CLI_HOME}/resources/tmux.conf
source/payload release/release.json        → ${SN_CLI_HOME}/resources/release.json
```

仓库 source 与 release archive payload 使用完全相同的左侧布局；archive binary
仍是根级 `sn-cli`、`sn-server`。activation 独占上述映射，active home 不反向充当
source/payload；任何不符合左侧精确布局的 payload 一律拒绝，不提供兼容 reader。
`resources/` 是 source/payload 的可扩展资产根，当前只有
`schema/`、`tools/`；未来 `skills/`、`mcp/` 也只能进入该根，不能重新占用仓库或
payload 顶层。`release/` 只保存 runtime、Tmux 与 activation identity 模板。

正式 release 的 Profile 文件集合由 `scripts/release-profile-files.sh` 显式列出；
正式 tool 集合由 `scripts/release-tool-files.sh` 显式列出；release asset 不按 glob
收集未登记文件。根目录 `make install`、doctor 和 provider smoke 仍按 source
配置工作，便于本地调试。

Profile loader 只读 active `configs/*.json`。每份文件必须用 `type=cli|api`
显式选择执行域；不存在 command ID 或第二层映射。loader 只接受本节列出的当前
字段，缺失领域标识或出现未知字段都会失败。

`resources/schema/profile.schema.json`、`runtime.schema.json` 和
`tool.schema.json` 是公开的 JSON
文档结构契约；Go loader 是涉及 adapter grammar、环境引用、跨字段关系、duration
上下限和文件系统事实的语义契约。两层都属于 normative validation，不能把 Schema
当作仅供编辑器提示的近似描述。仓库使用同一组 valid/invalid fixture 同时验证
Schema 与 loader；无法只由 JSON Schema 表达的上下文规则另有 loader 边界测试。
这类规则在 Schema 中使用 `x-sn-*` annotation 标出；标准 Draft 2020-12
validator 可以把 `format` 和自定义 annotation 当提示，但不能替代 Runtime
loader。测试保持单向不变量：任何 loader-valid 配置必须 schema-valid；只有写明
`semanticRule` 的 fixture 才允许“Schema 接受、loader 拒绝”。

## CLI Profile

```json
{
  "type": "cli",
  "command": "codex",
  "args": ["--skip-git-repo-check"],
  "env": {
    "CODEX_HOME": "${HOME}/.codex-aip",
    "REMOVE_ME": null
  },
  "model": "gpt-5.6-sol",
  "effort": "high",
  "prompt": "",
  "cwd": ""
}
```

- `command` 是单个可执行文件名或路径，不经过 shell。首期仅登记
  `codex`、`claude` adapter，按 `filepath.Base(command)` 选择；
- `args` 一字符串一 argv token。Profile 中的 option 必须在对应 adapter 的显式
  grammar 内，不能包含额外 positional prompt；
- `env` 继承启动环境后覆盖；`null` 删除变量；
- `${NAME}` 是唯一插值语法。`args/env/cwd` 从进程启动时的 inherited
  environment snapshot 展开一次，Profile `env` 条目不能彼此引用；
- `model`、`effort`、`prompt`、`cwd` 可省略；执行 mode 由公开 namespace 固定，
  不属于 Profile 配置；旧 `exec` 字段属于未知字段并被严格拒绝；
- `cwd` 在实际调用时必须解析成可进入目录。CLI ingress 可按 caller cwd 解析相对
  路径；HTTP CLI executor 只能使用 absolute override 或 Profile absolute `cwd`；
- `prompt` 使用 file-or-text 规则：若输入指向现有 regular file，则安全读取文件；
  不存在时按普通字符串；symlink、非 regular file、无效 UTF-8、NUL 或超限失败。

`model` 和 `effort` 不直接拼接到配置尾部。adapter 会识别并替换
`args` 中同类 selector，重建 command/global options、mode selector、mode-only
options、`--` 和最终 prompt 的正确顺序；重复或无法安全归类的配置 fail closed。
所有 CLI Profile 都必须支持 Session canonical plan。Claude `--verbose` 会把
`--output-format json` 的单个 result object 改成逐轮数组，因此属于 canonical
incompatible 参数，`profile check` 会在执行前拒绝。

### CLI direct 与 exec typed 参数

```text
sn-cli <cli-profile-id> \
  [--model <model>] \
  [--effort <low|medium|high|xhigh|max>] \
  [--prompt <file-or-text>] \
  [--cwd <dir>] \
  [input]

sn-cli exec <id> \
  [--model <model>] \
  [--effort <low|medium|high|xhigh|max>] \
  [--prompt <file-or-text>] \
  [--cwd <dir>] \
  [input]
```

两种写法都只接受 CLI Profile，但 execution mode 不同：bare Profile 固定
interactive/direct，`exec` 固定 non-interactive。二者共享 Profile loader 和
typed option grammar，顶层写法不是 raw argv passthrough。
每个 option 最多一次。`model/effort/cwd` 覆盖 Profile 默认值；
`--prompt` 是追加输入，不覆盖 Profile prompt。最终 prompt 按以下顺序用换行连接
非空片段：

```text
Profile prompt → --prompt → piped stdin → positional input
```

`--` 后最多一个 input。每个输入片段和最终 prompt 上限为 128,000 bytes；Runtime 还会在
spawn 前校验单 token 与总 argv/env/指针预算。

CLI Profile 的两个入口都在校验后 process replacement：

- `sn-cli exec <id>`：prompt 必须非空；stdin 固定 `/dev/null`；
- `sn-cli <cli-profile-id>`：prompt 可为空；非空 prompt 仍是最终 argv token；
  stdin 不是 TTY 时重新绑定 `/dev/tty`，没有 controlling TTY 则失败。

leading global `--json` 不包装 CLI Profile 的 stdout/stderr/exit code。`exec`
namespace 只选择目标命令的 non-interactive mode，不表示后台运行；后台入队通过
拥有执行语义的 `--queue` 入口完成。

### 执行 namespace 与保留 ID

公开 namespace 先固定执行语义，Profile `type` 再做严格配对校验；不会根据 ID 前缀
猜测类型，也不存在通用 Profile 执行入口：

```text
sn-cli cx --model one-off-model "回复 OK"
sn-cli exec cx --model one-off-model "执行任务"
sn-cli req api-cc "回复 OK"
```

bare Profile 和 `exec` 只接受 `type=cli`，`req` 只接受 `type=api`。Profile ID 紧跟
拥有它的 namespace，option 位于其后，input 必须是最后一个参数。

固定根 namespace `exec|req|profile|session|tmux|agent|run|server|help|version`、Profile
管理 action `list|show|check` 是保留 Profile ID。`configs/` 出现同名文件时
loader fail closed；其余合法 ID 都可以作为 Profile 名称。

`profile list|show|check` 是管理 action。`show` 不展开 secret；`check` 只做静态、
符号化验证，不解析真实 env/PATH/cwd，不读取 prompt file，也不调用 Provider。
`profile` namespace 不执行 Profile。这些管理动作和直接执行入口不加载
`runtime.json`；无关的 Agent、
scheduler 或 Run 配置错误不会阻止一次 Profile 查询或调用。

## API Profile

API Profile 保持独立 schema，不增加 CLI-only 字段。openai driver 示例：

API Profile 只支持下表字段；配置使用严格 JSON，不能加入 `//`、`/* */` 或
`#` 注释，未列出的字段会被 loader 拒绝：

| 字段 | 必填 / 适用 driver | Runtime 语义 |
| --- | --- | --- |
| `type` | 必填 | 固定为 `api`，选择 API Profile 领域 |
| `driver` | 必填 | `openai` 或 `anthropic`，选择 wire driver |
| `endpoint` | 与 `base_url` 二选一 | 完整 HTTPS endpoint，必须包含显式非根路径；Runtime 原样请求 |
| `base_url` | 与 `endpoint` 二选一 | HTTPS 基础地址；保留已有路径前缀，并按 driver 拼接默认 API 路径 |
| `model` | 必填 | 发送给 Provider 的模型名 |
| `headers.<name>` | 可选 | 承载所有 HTTP header，值可用 `${VAR}` 引用环境变量（runtime 调用时展开）。openai driver 对裸 `Authorization` 值（不含空格）自动补 `Bearer ` 前缀，anthropic 不补。secret 只存 `${VAR}` 引用名，不存值 |
| `parameters.max_tokens` | OpenAI 可选；Anthropic 必填 | 统一默认输出上限；adapter 分别映射为 `max_completion_tokens` 或 `max_tokens` |
| `parameters.temperature` | 可选；两者 | 默认采样温度，范围 `0..2` |
| `parameters.top_p` | 可选；取决于目标模型 | 默认 nucleus sampling 概率，范围 `0..1`；部分新模型不支持 |
| `parameters.stop_sequences` | 可选；取决于目标模型 | 默认停止序列，最多 4 个非空字符串；OpenAI 映射为 `stop` |
| `timeout` | 必填 | 单次 Provider attempt 超时，Go duration，范围 `(0, 24h]` |
| `context.window_tokens` | 可选 | Session 本地总上下文容量；省略或 `0` 时为 `32768` |
| `context.reserved_output_tokens` | 可选 | Session 本地输出预留；省略或 `0` 时基础默认值为 `8192` |
| `context.keep_recent_turns` | 可选 | 预留字段：仅校验、冻结并参与 digest，当前不影响 Session 历史裁剪 |
| `context.summary_enabled` | 可选 | 预留字段：仅解析、冻结并参与 digest，当前不触发历史摘要 |

当前 source API Profile 的字段使用情况：

| 字段 | `api-cc.json` | `api-cx.json` |
| --- | --- | --- |
| `type` | `api` | `api` |
| `driver` | `anthropic` | `openai` |
| `endpoint` | 未设置；由 `base_url` 解析 | 未设置；由 `base_url` 解析 |
| `base_url` | `https://open.bigmodel.cn/api/anthropic`，拼接 `/v1/messages` | `https://…/compatible-mode`，拼接 `/v1/chat/completions` |
| `model` | `glm-5.2` | `qwen3.7-max` |
| `headers` | `{"x-api-key": "${Z_AI_API_KEY}"}`，anthropic 不补 scheme | `{"Authorization": "${ALIYUN_API_KEY}"}`，openai driver 自动补 `Bearer` |
| `parameters.max_tokens` | `16384` | `16384`，adapter 转为 wire `max_completion_tokens` |
| `parameters.temperature` | 未设置；请求未覆盖时不发送 | 未设置；请求未覆盖时不发送 |
| `parameters.top_p` | 未设置；请求未覆盖时不发送 | 未设置；请求未覆盖时不发送 |
| `parameters.stop_sequences` | 未设置；请求未覆盖时不发送 | 未设置；请求未覆盖时不发送 |
| `timeout` | `50m` | `5m` |
| `context.window_tokens` | `1048576` | 未设置；使用保守默认 `32768` |
| `context.reserved_output_tokens` | `16384` | 未设置；受默认输出上限抬高，有效值为 `16384` |
| `context.keep_recent_turns` | 未设置（预留字段） | 未设置（预留字段） |
| `context.summary_enabled` | 未设置（预留字段） | 未设置（预留字段） |

```json
{
  "type": "api",
  "driver": "openai",
  "base_url": "https://example.invalid/provider",
  "model": "example",
  "headers": {
    "Authorization": "${MODEL_API_KEY}"
  },
  "parameters": {
    "max_tokens": 16384,
    "temperature": 0.2,
    "stop_sequences": ["END"]
  },
  "timeout": "5m",
  "context": {
    "window_tokens": 32768,
    "reserved_output_tokens": 8192,
    "keep_recent_turns": 8,
    "summary_enabled": false
  }
}
```

anthropic driver 使用 Messages API 原生字段：

```json
{
  "type": "api",
  "driver": "anthropic",
  "base_url": "https://example.invalid/provider",
  "model": "example",
  "headers": {
    "x-api-key": "${MODEL_API_KEY}"
  },
  "parameters": {
    "max_tokens": 16384,
    "temperature": 0.2
  },
  "timeout": "5m"
}
```

`endpoint` 与 `base_url` 必须且只能配置一个。`base_url` 不允许 query 或 fragment；
openai driver 默认拼接 `/v1/chat/completions`，anthropic driver 默认拼接
`/v1/messages`。例如 `https://example.invalid/provider` 会保留 `/provider` 前缀。
需要非默认路径或 query 时使用显式 `endpoint`。

`headers` 承载所有 HTTP header，值可用 `${VAR}` 引用环境变量；runtime 调用时从执行
环境展开。openai driver 对裸 `Authorization` 值（不含空格）自动补 `Bearer ` 前缀，
anthropic 不补。认证 header（`Authorization`、`x-api-key` 等）现在就写在 `headers`
里。`req` 与 `session req` 共同支持：

```text
共有: --max-tokens <n> --temperature <0..2>
req only: --system <text> --stream --request-file <path|->
```

`temperature`、`top_p` 与 `stop_sequences` 都只在 Profile 或 request 显式配置时发送；
具体 Provider 和模型可能进一步限制这些参数。通常只调整 `temperature` 或 `top_p`
之一，避免同时改变两个采样维度。

`context.window_tokens` 是 Session 本地投影使用的总上下文容量，不会发送给
Provider。省略或设为 `0` 时使用保守默认 `32768`，ContextManifest 记录
`capacity_source=conservative_default`；显式正值记录为 `profile`。
`context.reserved_output_tokens` 省略或设为 `0` 时默认 `8192`。实际输出预留取
该值、Profile 默认输出上限以及请求级输出上限中的最大值，因此较低的请求 override
不会扩大输入预算，较高值会收紧输入预算。输入预算计算为
`input_budget_tokens = window_tokens - effective_reserved_output_tokens`，且必须至少
保留 `2` 个输入 Token。

`--request-file` 读取 Runtime canonical `ModelRequest`，不是原始 Provider
payload。Driver 内部使用 Provider streaming 构建 canonical event，`--stream`
只控制 CLI 输出。

Durable Agent 创建 Run 时会把完整 API Profile、Profile digest 和 concrete
Provider driver semantic identity 冻结进 Store-private execution snapshot。
headers 只保存 `${VAR}` 引用名（header 名 + 环境变量名），不保存 secret 值；runtime
调用时从执行环境展开，resolved secret value 不持久化也不参与 digest；相同 `${VAR}`
引用名下轮换 secret 不构成 drift。`base_url`、`endpoint`、model、headers、parameters、
context、timeout、driver 或 `${VAR}` 引用名变化都会改变 snapshot。这里的 current
config 指当前执行进程已经加载的 Profile，不表示每轮从磁盘重新读取文件。

## Session 与 Tmux

execution mode 由入口固定，不读取 Profile mode 字段：

- `sn-cli session exec <cli-profile> ... <input>` 使用 non-interactive managed
  subprocess，捕获 canonical stdout/stderr/exit；CLI Turn override 只支持
  `--model`、`--effort`、`--cwd`；
- `sn-cli session req <api-profile> ... <input>` 执行一次 API request；
- 两条 Session 执行入口在 Profile ID 后使用 `--queue` 时创建 durable Run；
- `sn-cli session open <cli-profile> ...` 冻结 CLI Profile/base-prompt digest 与
  effective model/effort/cwd，启动 tmux-backed console；每个已消费 prompt 都创建 durable
  Session Run；
- `sn-cli tmux start <cli-profile> ... [input]` 固定 interactive mode，在专用 tmux
  server 的 `sn-session` 中创建一个 window。

Session 公开输入只来自 piped stdin 和最后一个位置参数；CLI Turn override 仅限
`--model`、`--effort` 和 `--cwd`。
Tmux 管理详见 [Tmux 管理契约](tmux-contract.md)。

`session list|show|messages|events|logs|executions|execution|reconcile`
与 `session configure|export|delete|gc` 只加载 Session maintenance service，
不依赖 Profile 目录、Provider 或 `runtime.json`。只有 `session exec|req`
和 `session open` 加载 Profile；terminal helper 使用仅含 Session executor 的窄
Session Run composition，不加载 Agent tools。

Run 同样按 action 最小加载：`get|list|result|events|watch` 只需要 SQLite Run
Store；`cancel|reconcile` 只需要 Run Store 与 Session maintenance service，不依赖
current Profile、Provider、tool 或 `runtime.json`；`gc` 只在省略
`--older-than` 时额外读取 retention 配置。`resume|retry`、带 `--queue` 的
`session exec|req`、`agent [--queue]` 和 worker execution 才加载完整执行依赖。Agent Retry
使用原 private snapshot 与 current
loaded snapshot 比较，不按当前配置生成一份新 snapshot。

## Tool manifest

外部 Agent tool 位于 active `${SN_CLI_HOME}/tools/<name>.json`，source/payload
位于 `resources/tools/<name>.json`。文件 basename 必须与 `name` 完全一致；接受
`schema_version=1`、`executor.type=mcp`，`effect` 为
`read_only`/`write_local`/`write_external` 三档之一（写副作用必须声明 `risk`，
`read_only` 缺省视为 `low`）。例如：

```json
{
  "schema_version": 1,
  "name": "web_search",
  "effect": "read_only",
  "risk": "low",
  "description": "Search web information.",
  "input_schema": {
    "type": "object",
    "properties": {
      "search_query": {"type": "string"}
    },
    "required": ["search_query"],
    "additionalProperties": false
  },
  "executor": {
    "type": "mcp",
    "endpoint": "https://open.bigmodel.cn/api/mcp/web_search_prime/mcp",
    "remote_tool": "web_search_prime",
    "headers": {
      "Authorization": "Bearer ${Z_AI_API_KEY}"
    },
    "timeout": "30s",
    "max_response_bytes": 1048576
  }
}
```

当前 release 固定交付两份定义：

| local name | MCP endpoint | remote tool | auth |
| --- | --- | --- | --- |
| `web_search` | `https://open.bigmodel.cn/api/mcp/web_search_prime/mcp` | `web_search_prime` | `Bearer ${Z_AI_API_KEY}` |
| `web_fetch` | `https://open.bigmodel.cn/api/mcp/web_reader/mcp` | `webReader` | `Bearer ${Z_AI_API_KEY}` |

`description` 与 `input_schema` 是注入模型并冻结进 Run snapshot 的 canonical
定义；Runtime 不根据远端返回临时改写 schema。tool name 必须匹配
`^[A-Za-z][A-Za-z0-9_-]{0,63}$`，远端工具名使用同一 grammar；`timeout` 范围
`1s..2m`，`max_response_bytes` 范围 `1024..8388608`。endpoint 必须是 absolute
HTTP/HTTPS URL，不得包含 userinfo、fragment 或环境引用；MCP protocol-owned header
不能由 manifest 覆盖，`Authorization` 必须严格使用 `Bearer ${VAR_NAME}`。

每个实际执行创建一个有界 MCP Streamable HTTP session，依次完成
`initialize`、`notifications/initialized` 和一次 `tools/call`；不 retry、不跟随
redirect。配置/网络/HTTP/JSON-RPC/protocol 错误作为
`ToolResult{is_error=true}` 闭合这个只读 effect，供 Agent 下一轮判断，不升级为未知
写副作用。secret 只在调用瞬间展开；manifest、snapshot、event 和错误不保存明文。
`server doctor` 只检查已启用工具的环境引用是否存在，不调用远端服务。

Tool manifest 只供 `sn-cli agent <api-profile-id>` 的自动 model/tool loop 使用。
`sn-cli req` 仍严格执行一次 Provider request；`sn-cli session req` 遇到 model tool
call 仍进入 `requires_action`，两者都不会自动执行 `tools/` 中的 handler。

## runtime.json

```json
{
  "agent": {
    "tools": ["read_file", "list_directory", "web_search", "web_fetch"],
    "workspace_roots": ["/absolute/project"],
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
  }
}
```

文件缺失时使用代码默认值。存在时必须是 regular file、非 symlink、严格 JSON，
未知字段失败。`workspace_roots` 未配置时以当前启动 cwd 为唯一 root；默认 tool
全部只读，`write_file` 必须显式启用。`agent.tools` 名称先做 grammar 和去重校验，
bootstrap 再要求每个名称属于 builtin 或当前 Tool Catalog，未知名称 fail closed；
Tool Catalog 不得覆盖 builtin 名称。`tools` 和
`workspace_roots` 显式配置时必须是 array，不能写 `null`。duration 使用 Go
duration 语法，并额外满足：

- `agent.max_wall_time`: `1s..24h`；
- `scheduler.poll_interval`: `10ms..1m`；
- `run.settled_retention`: `1h..8760h`。

“文件缺失时使用默认值”只适用于普通 Runtime bootstrap。activation payload、
需要保留的 active 配置和 staged home 都使用 required loader：`runtime.json`
缺失、symlink 或在检查/open 间被替换时直接拒绝，不能回退为默认值。

Agent tool execution snapshot 冻结 enabled definitions、tool
implementation/semantic version，以及 canonical `workspace_roots`、cwd 和 root
device/inode identity。它与 model/Provider snapshot、Agent execution contract
version 共同形成 Run 的 combined `config_digest`；绑定 Session 时还单独保存
Session 自己的 profile-only request/config digest。新 model/tool side effect 前
current snapshot 必须完整相等，但恢复 durable terminal/effect、取消和 reconcile
不依赖 current tool configuration。implementation version 是人工执行契约；改变
未被 snapshot 其它字段表达的行为时必须同步 bump，不能用 build 或 release version
代替。

## 版本与激活 schema

contract v4 使用 Tool manifest `schema_version=1`、Session fact
`schema_version=2` 和 SQLite
`PRAGMA user_version=4`。Runtime 只读取这组完整 schema，不做版本推断、字段补齐或
自动 migration。安装前需要：

1. 停止 `sn-server` 和所有 `sn-cli tmux` managed window；
2. 确认 active Profile、Tool Catalog、Session fact 和 `state/runtime.db*` 都符合
   当前 schema；
3. 不需要保留的 unsupported state 应整体移到可恢复备份后重新初始化；
4. 需要全量替换 Profile、Tool Catalog 和 `runtime.json` 时显式使用
   `install.sh --overwrite-configs`；
5. 再执行当前 release 的 `install.sh`。

安装和 self-update 必须在替换 binary/resources 前完成 active-home preflight。
`--overwrite-configs` 只授权替换 Profile、Tool Catalog 和 `runtime.json`，不绕过
live server、Tmux、Session 或 Run
门禁，也不授权迁移或修改 unsupported Session/Run state。

根目录 `make install` 是独立的本地源码调试策略，不使用上述数据保留流程，也不
需要额外 Make 变量。它固定校验并安装完整 source bundle，自动安全停止受管
`sn-server`，把 source `configs/`、`resources/tools/` 和 `release/runtime.json`
分别覆盖到 active `configs/`、`tools/` 和 `runtime.json`，并在 activation
journal/guard 仍生效时删除
`sessions/`、`state/session-locks/`、`state/session-invocations/`、
`state/session-mutations/`、`state/session-trash-moves/` 和
`state/runtime.db{,-wal,-shm,-journal}`。Runtime artifact 完整提交并校验前不删除
这些状态；成功后不重启 server。该授权不传递给 archive installer 或
`server update`。
installer 会在任何目录创建前解析 canonical home/install-dir，并拒绝 install-dir
位于 Runtime home 内。尚不存在的路径组件只接受 printable ASCII，避免
case-insensitive filesystem 上无法在无写 dry-run 中证明安全的 Unicode alias；
已存在的 Unicode ancestor 不受影响。激活后的 command symlink 通过逐路径组件
no-follow 的 directory descriptor 管理，并再次强制位于 home 外。installer 在
activation mutation 前先持久化 `.sn-cli.<link-name>.owner.json`、持有其 `flock`
并以 no-clobber `symlinkat` 发布 exact command link；已有 regular file、目录或
非目标 symlink 均不替换。activation 失败或 reservation release 不删除 owner/link，
后续 retry 只能在 owner 内容、parent/link/owner inode 与 exact target 全部保持一致
时复用。

激活期间 `${SN_CLI_HOME}/state/activation.guard.json` 与 durable journal schema 3
保护 contract-v4 入口，active `bin/`、`configs/`、`tools/` 还会短暂替换为
regular-file barrier 以阻断
并发路径重建。恢复只接受 journal 中 exact artifact set、owner PID/start-token
与 original/staged/guard digest 一致的状态。journal 在
`committed|rolled_back` terminal phase 仍是入口 barrier，直到目录树、rename、
guard/journal 删除都按顺序 fsync 完成。

## Server

- `HTTP_ADDR`：监听地址，默认 `127.0.0.1:8080`；
- `SN_SERVER_TOKEN`：Bearer token；
- 非 loopback 地址必须设置 token；
- `SN_CLI_HOME`：active Runtime home。

`server start` 只启动 `${SN_CLI_HOME}/bin/sn-server`。PID identity、process lease、
lifecycle/maintenance lock、日志和升级 guard 位于 `${SN_CLI_HOME}/state/`。
首次启动和 server 已运行时，human 输出都包含 `pid=<正整数>`；leading-global
JSON 调用 `sn-cli --json server start` 固定返回 `running=true` 和同一个正整数
`pid`，供第三方托管。
