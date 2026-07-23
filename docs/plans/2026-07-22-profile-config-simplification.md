# Profile 配置简化实施记录

日期：2026-07-22

## 结论

公开 Provider JSON 收口为两个扁平 family：

- CLI：`command` 必填；`model/effort/args/env/timeout_seconds` 可选。
- API：`protocol/base_url/model/api_key` 必填；`headers/timeout_seconds` 可选。

profile ID 继续取文件名。`type` 与 `cli/api` wrapper 删除；普通加载严格、只读且拒绝未知字段。

## 已实施决策

1. `native` 移出公开 profile schema；内部实现仅保留兼容代码与注入测试，不可从 `configs/*.json` 加载。
2. profile 内 `executor/tmux` 删除；tmux 统一由 `session open --carrier tmux` 或内部 command/session 转换创建。
3. `prompt_delivery/prompt_args` 删除；CLI prompt 发送策略由 adapter 固定管理。
4. `env_passthrough/env_unset` 合并到 `env`：`"${NAME}"` 表示显式读取父环境，`null` 表示删除。
5. `override_policy` 删除；Runtime typed overrides 保留，并始终按 CLI/API adapter 支持列表校验。
6. `depends/execution` 删除；profile 不再拥有依赖进程与 proxy/shim 编排。
7. API 的 `auth/stream/mock/runtime` 删除；`headers` 保留为 profile-only 固定字段，认证方式由 `api_key`、协议与 endpoint 推导。
8. `args` 是唯一固定 argv 字段，每个数组元素对应一个 argv token；不再提供 `managed_args`。

## 兼容与迁移

`sn-cli system migrate-config` 是唯一旧 schema 写入入口。它会：

- 展开 embedded presets 为独立 JSON；同名独立文件优先。
- 扁平化旧 `type/cli/api`、`driver/binary`、嵌套 `command/runtime`。
- 删除默认 `managed_args`、`api.auth`、`result_contract` 与 profile 级 override policy。
- 合并 `env/env_passthrough/env_unset`。
- 对旧 native、profile tmux、depends/execution、非默认 prompt 与 API 高级运行字段明确报错。

普通 `LoadDir` 不做隐式迁移或文件写入。

## 保持不变

- HTTP/Go 结构化 typed overrides 保留；CLI profile 尾参数改为原样透传，API CLI 入口只接受固定 typed options。
- Codex/Claude 的显式 managed execution 自动增加 `exec`/`-p`；native direct 不增加，`args` 已包含对应 token 时不重复。
- Session 的 tmux/terminal carrier、recording 与 transcript 契约。
- `runtime.yaml` 的队列、deadline 与 Session carrier 设置。
- `resources/{personas,skills,tools,schema}` 的资源目录边界。

## 模板结果

仓库保留 11 个 profile：`cx`、`cc`、`api-cx`、`api-cc`、`cx-image`、`cx-spark`、`commit`、`cc-bai`、`cc-glm`、`mcx`、`mcc`。模型、参数、环境变量与超时值保持不丢失；`cc-bai/cc-glm` 的 `ANTHROPIC_AUTH_TOKEN` 改为 `env` 中的 `null`。

## 验收门禁

```bash
go test ./...
go vet ./...
make sn-cli-test
make release-check
git diff --check
```
