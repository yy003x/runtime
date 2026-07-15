# AGENTS.md

## Project Description

本仓库实现一个自包含的 Go Agent Runtime，并以 `sn-cli` 作为统一终端入口。

当前目标是与 `/Users/yang/ai-workbench/wb/runtime` 的公开能力和运行契约保持一致，包括：

- 通过 `configs/<profile_id>.json` 定义 CLI、API 和 tmux provider。
- 支持 Codex、Claude、OpenAI-compatible 和 Anthropic-compatible adapter。
- 支持 provider presets、typed overrides、配置校验和环境变量展开。
- 支持 managed/capture 执行、task/turn/loop/session 生命周期。
- 支持 request、status、events、logs、output、result 等运行产物契约。
- 支持 profile、doctor、config validate、watch、cancel 和 capabilities 命令面。
- `sn-cli <profile_id>` 由当前仓库直接实现，不依赖外部 `SINAN_ROOT` runtime。

## Project Rules

遵循用户级 `AGENTS.md` 约定和用户在当前任务中的明确指令。

修改 `sn-cli` 顶层命令、profile 分流、`--` 语义或 direct/managed 边界前，必须先读取 `docs/cli-routing-contract.md`。实现、测试、`sn-cli --help`、README 和架构文档必须按该契约的变更门禁同步更新，不得只修改其中一处。
