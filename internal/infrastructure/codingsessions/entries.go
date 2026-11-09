// Package codingsession 定义 sn-code 交互会话的 append-only JSONL 条目模型。
// 结构语义对齐 pi 的 session 格式：首行 header，之后每行一个 entry，
// entry 通过 id/parentId 构成树，当前分支由 leafId 决定。
package codingsessions

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/yy003x/sn-code/pkg/contract"
)

// Version 是当前会话文件格式版本，对应 pi 的 session version 3。
const Version = 3

// Header 是会话文件首行；后续所有 entry 的 parentId 链最终指向 header 之后
// 的第一个 entry（parentId 为空）。
type Header struct {
	Type          string    `json:"type"`
	Version       int       `json:"version"`
	ID            string    `json:"id"`
	Timestamp     time.Time `json:"timestamp"`
	CWD           string    `json:"cwd"`
	ParentSession string    `json:"parentSession,omitempty"`
}

// EntryType 枚举 v1 支持的条目类型。pi 的 context_edit、custom_message、
// label、thinking_level_change 暂不写入，读取时按未知类型跳过。
type EntryType string

const (
	// EntryTypeMessage 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EntryTypeMessage EntryType = "message"
	// EntryTypeModelChange 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EntryTypeModelChange EntryType = "model_change"
	// EntryTypeCompaction 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EntryTypeCompaction EntryType = "compaction"
	// EntryTypeBranchSummary 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EntryTypeBranchSummary EntryType = "branch_summary"
	// EntryTypeCustom 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EntryTypeCustom EntryType = "custom"
	// EntryTypeSessionInfo 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EntryTypeSessionInfo EntryType = "session_info"
)

// Entry 是条目的公共骨架与载荷。Message 之外的自定义载荷均为 sn-code 私有
// JSON，不保证跨版本稳定。
type Entry struct {
	Type      EntryType `json:"type"`
	ID        string    `json:"id"`
	ParentID  string    `json:"parentId"`
	Timestamp time.Time `json:"timestamp"`

	Message     *contract.Message `json:"message,omitempty"`
	ModelChange *ModelChange      `json:"modelChange,omitempty"`
	Compaction  *Compaction       `json:"compaction,omitempty"`
	Custom      *Custom           `json:"custom,omitempty"`
	SessionInfo *SessionInfo      `json:"sessionInfo,omitempty"`
}

// ModelChange 记录一次 profile 切换。sn-code 的模型身份是 API Profile ID，
// 不拆 provider/modelId。
type ModelChange struct {
	Profile string `json:"profile"`
}

// Compaction 记录一次上下文压缩：firstKeptEntryID 之前的同分支条目不再
// 进入模型上下文，由 summary 摘要替代。
type Compaction struct {
	Summary          string `json:"summary"`
	FirstKeptEntryID string `json:"firstKeptEntryId"`
	TokensBefore     int64  `json:"tokensBefore"`
}

// Custom 保存调用方私有状态，不进入模型上下文。
type Custom struct {
	CustomType string          `json:"customType"`
	Data       json.RawMessage `json:"data,omitempty"`
}

// SessionInfo 保存会话显示名。
type SessionInfo struct {
	Name string `json:"name,omitempty"`
}

// Validate 校验条目结构与类型载荷的一一对应。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (entry Entry) Validate() error {
	if entry.ID == "" {
		return fmt.Errorf("session entry id is empty")
	}
	var payloads int
	for _, present := range []bool{
		entry.Message != nil,
		entry.ModelChange != nil,
		entry.Compaction != nil,
		entry.Custom != nil,
		entry.SessionInfo != nil,
	} {
		if present {
			payloads++
		}
	}
	if payloads != 1 {
		return fmt.Errorf("session entry %s must carry exactly one payload", entry.ID)
	}
	switch entry.Type {
	case EntryTypeMessage:
		if entry.Message == nil {
			return fmt.Errorf("message entry %s missing message payload", entry.ID)
		}
	case EntryTypeModelChange:
		if entry.ModelChange == nil || entry.ModelChange.Profile == "" {
			return fmt.Errorf("model_change entry %s missing profile", entry.ID)
		}
	case EntryTypeCompaction:
		if entry.Compaction == nil || entry.Compaction.FirstKeptEntryID == "" {
			return fmt.Errorf("compaction entry %s missing firstKeptEntryId", entry.ID)
		}
	case EntryTypeBranchSummary, EntryTypeCustom:
		if entry.Custom == nil {
			return fmt.Errorf("custom entry %s missing custom payload", entry.ID)
		}
	case EntryTypeSessionInfo:
		if entry.SessionInfo == nil {
			return fmt.Errorf("session_info entry %s missing sessionInfo payload", entry.ID)
		}
	default:
		return fmt.Errorf("unknown session entry type %q", entry.Type)
	}
	return nil
}

// DecodeLine 解析一行 JSONL。未知类型按 pi 语义跳过（返回 ok=false），
// 已知类型解码失败时报错，避免半解析状态进入投影。
// 参数 line 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *Header、*Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func DecodeLine(line []byte) (header *Header, entry *Entry, err error) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return nil, nil, nil
	}
	var probe struct {
		Type    string `json:"type"`
		Version int    `json:"version"`
	}
	if err := json.Unmarshal([]byte(trimmed), &probe); err != nil {
		return nil, nil, fmt.Errorf("decode session line: %w", err)
	}
	if probe.Type == "session" {
		var decoded Header
		if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
			return nil, nil, fmt.Errorf("decode session header: %w", err)
		}
		if decoded.ID == "" {
			return nil, nil, fmt.Errorf("session header id is empty")
		}
		return &decoded, nil, nil
	}
	var decoded Entry
	if err := json.Unmarshal([]byte(trimmed), &decoded); err != nil {
		return nil, nil, fmt.Errorf("decode session entry: %w", err)
	}
	switch EntryType(decoded.Type) {
	case EntryTypeMessage, EntryTypeModelChange, EntryTypeCompaction,
		EntryTypeBranchSummary, EntryTypeCustom, EntryTypeSessionInfo:
		if err := decoded.Validate(); err != nil {
			return nil, nil, err
		}
		return nil, &decoded, nil
	default:
		// 未知类型跳过：未来版本写入的新条目不应让旧读取者失败。
		return nil, nil, nil
	}
}
