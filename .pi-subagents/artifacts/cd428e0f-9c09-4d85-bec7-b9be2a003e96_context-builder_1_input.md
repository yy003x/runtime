# Task for context-builder

只读分析。完整梳理 `wb-chat` 当前真实实现，而不只读 skill 文档：从 `skills/wb-chat/**` 追踪到 `wb/agent/chat_router.py`、skill router/index/execution、相关配置/schema、tests、routing evaluation、TaskRecord envelope 与 interaction mode。特别识别 effort/reasoning/profile 当前在哪里决定、是否已有动态机制、路由和执行 owner 的边界、可插入点、不可破坏契约。仓库很脏，绝不修改或格式化。引用文件与行号，给出风险和测试入口。

---
**Output:**
Write your findings to exactly this path: /Users/yang/go/run/runtime/.pi-subagents/artifacts/outputs/cd428e0f-9c09-4d85-bec7-b9be2a003e96/analysis/wb-chat-current.md
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