# SN Runtime 测试安全网

`internal/testkit/` 只保存 SN Runtime 的测试资产，不是生产实现或公开契约。
生产 `cmd/`、`internal/{domain,application,infrastructure,interfaces}`、`pkg/`
和 Workbench 入口均不依赖这里的 package。

目录职责：

- `ptyx/`：Darwin/Linux PTY 启动、进程组 signal 和 exit code 测试基座。
- `commandgolden/`：用 fake `codex/claude` 固化 `cx`、`cc` 和 `cx-*` 的
  argv、env、TTY、stdin、signal 与 exit 行为，并直接验证隐式/显式 Profile
  等价、active `configs/` 与 Command Bridge。
- `scenario/`：测试侧 canonical request/event/result/error fixture 与归一化器。
- `faux/`：单次 scripted provider 和本地 SSE/HTTP 场景端点，不包含 retry。

验证入口：

```bash
go test ./internal/testkit/...
go list -deps ./cmd/sn-cli
```

`commandgolden/testdata/profiles.json` 的 provenance（来源信息）明确区分 source
HEAD 和 installed build。只有当前用户可见 command 行为被有意修改时才能更新
argv/env golden；不得为了让回归测试通过而重新推导或放宽 token 顺序。

PTY 使用精确 pin 的 `github.com/creack/pty`。现有 `golang.org/x/sys/unix` 没有
可直接复用的跨 Darwin/Linux `OpenPTY/Grantpt/Ptsname` helper，自建实现会复制
平台 ioctl 细节。该依赖只能由 `internal/testkit/**` import，生产 binary 依赖图必须
保持不包含它。
