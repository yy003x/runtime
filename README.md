# Go Agent Runtime

本仓库实现一个自包含的 Go Agent Runtime，并以 `sn-cli` 提供统一终端入口。Runtime 直接读取 `configs/<profile_id>.json`，支持 CLI、API、tmux provider，以及 task、turn、loop、session、command 的完整运行生命周期；不依赖外部 `SINAN_ROOT` 或 Python runtime。

当前 provider 配置、运行目录契约和命令面与 `~/ai-workbench/wb/runtime` 保持一致。仓库原有的 persona、memory 与 HTTP agent service 继续保留。

## Runtime 能力

- JSON provider profile：`cli` / `api`、preset、`extends`、typed overrides、alias、环境变量展开。
- Provider adapter：Codex、Claude、OpenAI-compatible、Anthropic-compatible。
- 执行模式：`managed` 强制 `result.json` 完成契约，`capture` 根据 provider 输出合成结果。
- 生命周期：`task`、`turn`、`loop`、`session`、`command`，支持 status、logs、watch、cancel/interrupt/stop/attach。
- tmux：交互 session、prompt paste、command 托管与日志捕获。
- Capabilities：skill 发现/路由/执行、tool schema/Guardrail、JSON file memory。
- 运行产物：`request.json`、`status.json`、`events.jsonl`、`output.log`、`result.json`；tmux task 额外使用空 `done` 文件作为最终完成标记。
- 配置诊断：`profiles`、`config choices`、`config validate`、`doctor`、`prune`。

## Provider 配置

Runtime settings 位于 [configs/runtime.yaml](configs/runtime.yaml)，provider 只从 `configs/*.json` 加载。配置使用 Workbench JSON schema，不再兼容旧 YAML provider schema或 `.local.json` 覆盖。

仓库默认提供：

- `cx`、`cx-terra`、`cx-luna`、`cx-spark`、`cx-image`：Codex CLI 与 presets。
- `cc`：Claude CLI。
- `tcx`、`tcc`：Codex / Claude tmux session。
- `ba`、`bo`：百炼 Anthropic / OpenAI-compatible API。
- `ora`、`oro`：OpenRouter Anthropic / OpenAI-compatible API。
- `fake`：无远端依赖的本地 mock，用于测试。

最小 API profile：

```json
{
  "type": "api",
  "label": "My OpenAI-compatible Provider",
  "timeout_seconds": 300,
  "api": {
    "protocol": "openai",
    "base_url": "https://example.com/v1",
    "model": "model-id",
    "api_key_env": "MY_API_KEY"
  }
}
```

配置文件只引用 secret 的环境变量名，不能内联 token、password、cookie 等敏感字段。

## sn-cli

构建和安装：

```bash
make sn-cli-build
make sn-cli-install
sn-cli --help
```

按 profile 直接执行：

```bash
sn-cli fake "hello"
sn-cli cx "分析当前仓库"
sn-cli cx --prompt-file ./prompt.md --image ./screen.png
sn-cli cx --prompt_file ./prompt.md --image ./one.png --image ./two.png
sn-cli cx-spark --model gpt-5.3-codex-spark "review"
```

`<profile_id>` 的解析优先于 `sncli/conf/default.json` 中的旧工具 alias。`--prompt-file` 与 `--prompt_file` 等价；`--image` 可重复，作为 Codex typed override 透传。CLI 参数覆盖 profile/preset 的对应配置。

生命周期命令：

```bash
sn-cli profiles
sn-cli config choices
sn-cli config validate --profile fake
sn-cli doctor

sn-cli task run --profile fake --mode capture "处理任务"
sn-cli task status <run_id>
sn-cli task logs <run_id>
sn-cli task watch <run_id>
sn-cli task cancel <run_id>

sn-cli turn run --profile fake --prompt-file ./prompt.md
sn-cli loop run --input "执行计划" --actions '[{"type":"respond","content":"完成"}]'

sn-cli session start --profile tcx
sn-cli session send <run_id> --text "继续"
sn-cli session logs <run_id>
sn-cli session attach <run_id>
sn-cli session stop <run_id>

sn-cli command start --profile tcx -- printf 'hello'
sn-cli command status <run_id>
sn-cli command logs <run_id>
sn-cli command stop <run_id>

sn-cli capabilities tools schemas
sn-cli capabilities skills list --skills-dir ./skills
sn-cli capabilities memory demo --query runtime --items '[{"id":"1","type":"fact","content":"Go runtime","source":"demo"}]'
sn-cli prune
```

run 产物位于：

```text
runs/global/runtime/<task|turn|session|command>/<YYYY-MM-DD>/<run_id>/
runs/global/runtime/loop/<YYYY-MM-DD>/<loop_id>/
```

tmux task 只有在 `result.json` 已写入且 `done` 已创建后才进入结果校验；单独出现任一文件都不视为成功。

## 远程安装

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/scripts/install-sn-cli.sh | bash
```

脚本默认 clone/update 到 `~/.sn-cli/runtime`，构建当前仓库的 Go runtime，并安装 `~/.local/bin/sn-cli`。安装结束前会验证 `profiles` 和本地 `fake` provider 配置。

```bash
curl -fsSL https://raw.githubusercontent.com/yy003x/runtime/main/scripts/install-sn-cli.sh | \
  SN_CLI_REF=main bash
```

更多安装与更新选项见 [sncli/README.md](sncli/README.md)。

## HTTP Agent Service

原有 HTTP service 读取 `configs/config.yaml` 与 `configs/personas/*.yaml`，提供：

- `POST /v1/agents`
- `POST /v1/chat`
- `GET /v1/sessions/{session_id}/memory`

启动与测试：

```bash
make run
make test
```

默认监听 `:8080`。当前 memory store 为进程内实现，retrieval memory 仍是 stub。

## 项目结构

- `internal/provider`：Workbench JSON profile、preset、CLI/API adapter 与 executor。
- `internal/agentrun`：run contract、状态机、tmux、loop、command、registry。
- `internal/capability`：skills、tools、memory。
- `sncli/cmd/sn-cli`、`sncli/internal`：终端入口、REPL、安装更新支持。
- `internal/agent`、`internal/memory`、`internal/transport`：原有 HTTP agent service。
- `configs`：runtime settings、provider profiles、HTTP service 与 persona 配置。
