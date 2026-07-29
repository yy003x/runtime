# CLI Profile 与 Session Command 协议 Preparation Brief

## 当前状态

- readiness：superseded
- 用户授权：本文件只保存历史讨论，不是当前契约或实施依据
- 后续决策：当前正式契约以
  `documents/designs/contracts/profile-session-tmux-protocol.md` 为准。其中
  `configs/*.json` 是唯一 Profile 配置层，`sn-cli <profile-id>` 完全等价于
  `sn-cli profile <profile-id>`，并由 `type=cli|api` 选择 adapter；不存在
  command shortcut 或 raw/native argv passthrough

以下内容保留当时讨论快照，其中的未决项和与正式契约冲突的结论均已失效。

## 已确认上下文

- 目标：把 `sn-cli profile <id>` 收敛为 typed 参数入口，由 Runtime 根据 Profile
  配置和 command adapter 生成唯一、顺序正确的最终 argv。
- CLI Profile 配置使用 `command` 替代 `binary`；adapter 根据 `command` 查找，
  未登记的 command 在执行前报错，不再配置 `effort_adapter`。
- CLI Profile 取消 `transport`、`prompt_delivery`；使用配置级 `launch` 表达
  `tty|terminal|tmux`，但 `sn-cli profile` 不提供动态 `--launch`。
- `launch` 只决定完整命令在哪里运行，不改变命令内容。所有启动方式都先生成完整
  command，最终 prompt 始终作为最后一个 argv token。
- `sn-cli profile` 的 typed 参数为 `model`、`effort`、`prompt`、`exec`、`cwd`；
  不支持 `--interactive`。`exec` 需要支持显式 true/false，以便 CLI 覆盖配置。
- prompt 的输入顺序为：配置或 `--prompt`、piped stdin、最后一个位置参数；合并后
  作为一个最终参数。`--prompt` 值若对应现有文件则读取文件，否则按文本处理。
- `exec=true` 时最终 prompt 不得为空；`exec=false` 且 prompt 为空时直接启动
  交互命令。
- adapter 负责识别 command 参数、exec selector 和 subcommand 参数，处理配置
  `args` 中已有的 `model`、`effort`、`exec` 与 typed override 的冲突，保证最终
  顺序为 `command + command args + exec selector + subcommand args + prompt`。
- typed override 优先于 Profile 配置；顶层 `sn-cli <command-id>` 继续保持 native
  argv 透明透传，不改成 typed Profile 入口。
- Profile 与 Session 必须是物理隔离的两套实现，即使字段或行为相同也不复用解析
  和执行状态机。本轮不实施 Session 修正。
- Session 的当前讨论方向是：API Profile 保持 HTTP model call；CLI Profile 使用
  tmux carrier 和 paste。用户接受强制 `exec=false`，也接受运行时转成
  `exec=false`，两者尚未最终选定。
- Session 的 `--prompt-file` 保留，不改名为 `--session-file`；它与 Profile 的
  `--prompt` 位于不同 namespace，不构成冲突。

## 明确约束与非目标

- 配置保持简单，不增加 raw passthrough、`manual`、`argv` 或 `prompt_delivery`
  选择层。
- `command` 是可执行命令标识，不是一段经 shell 解析的命令字符串；`args` 仍是一
  字符串一 argv token，`env` 继续独立解析。
- 不恢复旧 `profile exec|open`、无 `type` Profile、旧 namespace 或兼容 shim。
- 当前阶段不修改源码、schema、source configs、active `${SN_CLI_HOME}`、Session
  artifact、Git 历史或发布物。

## 已补事实

| 事实 | 来源 | 对后续的影响 |
| --- | --- | --- |
| 当前 `command.Profile` 仍包含 `Binary/Transport/PromptDelivery/EffortAdapter` | `command/profile.go` | 字段清理会同时影响 loader、schema、catalog、bridge、Runner、测试和文档 |
| 当前 Session 直接调用 `command.Runner`，不是物理隔离实现 | `session/service.go`、`internal/runtimebootstrap/vnext.go` | Session 修正需要独立设计，不能把 Profile 改动当成纯字段重命名 |
| Session 依赖 transport 选择执行方式、记录 execution，并控制 tmux carrier | `command/execution.go`、`session/types.go`、`internal/cli/session_vnext.go` | 删除 transport 配置前必须确定 Session 自己的 launch 与持久化语义 |
| Session 当前依赖 prompt delivery 做 argv/stdin/paste/manual 分流 | `command/execution.go`、`session/service.go` | 新 Session 若固定 paste，应由 Session 自己实现和校验 |
| `session run|submit` 当前支持 CLI 与 API Profile，并由同一 Profile catalog 加载 | `internal/cli/session_vnext.go`、`profile/catalog.go` | 任一 command adapter 校验失败可能阻断全部 Session 管理入口，需要在设计中决定校验时机 |

## 用户偏好

- 已明确：Profile 协议保持独立、配置驱动、参数少、最终 argv 可预测。
- 已明确：Session 与 Profile 即使能力相似也应物理隔离。
- 已明确：先记录上下文，后续再共同完成正式 design，不在本轮实施。
- 待确认推断：Session CLI Profile 最终固定为
  `launch=tmux + exec=false + paste`，并删除 Session 的 terminal 相关入口。

## 剩余阻塞项

- Session CLI Profile 的最终 effective contract 尚未闭合：对非
  `launch=tmux` 或 `exec=true` 的配置，是严格拒绝，还是由 Session 强制投影为
  `launch=tmux + exec=false`。该决策会改变配置真实性、错误行为和 Session
  adapter 的职责。

## 下一步

- 后续设计开始时先回读本 brief，并重新核对当前源码、schema、source/active
  Profile 和真实 CLI help；先闭合 Session effective contract，再写
  `documents/designs/pending/` 正式设计。正式设计确认后才进入实施。
