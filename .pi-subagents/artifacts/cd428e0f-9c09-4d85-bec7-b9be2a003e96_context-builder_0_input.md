# Task for context-builder

只读分析。完整审计外部仓库 `/var/folders/md/4l8j_bn96m39th1mjcpd4bj00000gn/T/tmp.trcOFv2YI4/repo` 的 `pi-adaptive-thinking` 实现与测试。目标：逐项说明所有用户可见功能、配置、状态机、事件生命周期、effort/thinking level 解析与切换、临时/持久语义、并发锁、安全/失败行为、测试覆盖、限制与可迁移设计原则。必须读取全部 src、测试、README、CHANGELOG、package 配置；引用文件与行号。不要修改任何文件。输出适合后续架构设计的证据报告。

---
**Output:**
Write your findings to exactly this path: /Users/yang/go/run/runtime/.pi-subagents/artifacts/outputs/cd428e0f-9c09-4d85-bec7-b9be2a003e96/analysis/adaptive-thinking.md
This path is authoritative for this run.
Ignore any other output filename or output path mentioned elsewhere, including output destinations in the base agent prompt, system prompt, or task instructions.

## Acceptance Contract
Acceptance level: checked
Completion is not accepted from prose alone. End with a structured acceptance report.

Criteria:
- criterion-1: Return concrete findings with file paths and severity when applicable

Required evidence: changed-files, tests-added, commands-run, residual-risks, no-staged-files

Finish with a fenced JSON block tagged `acceptance-report` in this shape:
Use empty arrays when no items apply; array fields contain strings unless object entries are shown.
`criteriaSatisfied[].status` must be exactly one of: satisfied, not-satisfied, not-applicable.
`commandsRun[].result` must be exactly one of: passed, failed, not-run.
`manualNotes` and `notes` are optional strings; an empty string means no note and does not satisfy `manual-notes` evidence.
```acceptance-report
{
  "criteriaSatisfied": [
    {
      "id": "criterion-1",
      "status": "satisfied",
      "evidence": "specific proof"
    }
  ],
  "changedFiles": [
    "src/file.ts"
  ],
  "testsAddedOrUpdated": [
    "test/file.test.ts"
  ],
  "commandsRun": [
    {
      "command": "command",
      "result": "passed",
      "summary": "short result"
    }
  ],
  "validationOutput": [
    "validation output or concise summary"
  ],
  "residualRisks": [
    "none"
  ],
  "noStagedFiles": true,
  "diffSummary": "short description of the diff",
  "reviewFindings": [
    "blocker: file.ts:12 - issue found, or no blockers"
  ],
  "manualNotes": "anything else the parent should know"
}
```