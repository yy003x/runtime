package codingsessions

import (
	"fmt"
	"slices"
	"sort"

	"github.com/yy003x/sn-code/pkg/contract"
)

// Path 从 leafID 沿 parentId 回溯到根并反转，得到当前分支的线性条目序列。
// entries 必须同属一个会话文件且 id 唯一；leafID 为空时取最后一个条目。
// 参数 entries、leafID 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func Path(entries []Entry, leafID string) ([]Entry, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	byID := make(map[string]*Entry, len(entries))
	for index := range entries {
		entry := &entries[index]
		if _, duplicate := byID[entry.ID]; duplicate {
			return nil, fmt.Errorf("duplicate session entry id %s", entry.ID)
		}
		byID[entry.ID] = entry
	}
	leaf := leafID
	if leaf == "" {
		leaf = entries[len(entries)-1].ID
	}
	current, found := byID[leaf]
	if !found {
		return nil, fmt.Errorf("session leaf %s not found", leaf)
	}
	path := make([]Entry, 0, len(entries))
	for current != nil {
		path = append(path, *current)
		if current.ParentID == "" {
			break
		}
		parent, exists := byID[current.ParentID]
		if !exists {
			return nil, fmt.Errorf("session entry %s references missing parent %s", current.ID, current.ParentID)
		}
		current = parent
	}
	for left, right := 0, len(path)-1; left < right; left, right = left+1, right-1 {
		path[left], path[right] = path[right], path[left]
	}
	return path, nil
}

// ContextEntries 构建进入模型上下文的条目序列：路径上最新一次 compaction
// 生效，其 firstKeptEntryID 之前的条目被 summary 替代。系统提示词不在
// 消息列表中（由运行时每次请求重建，经 ModelRequest.System 传递）。
// 参数 path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []Entry 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func ContextEntries(path []Entry) []Entry {
	compactionIndex := -1
	for index, entry := range slices.Backward(path) {
		if entry.Type == EntryTypeCompaction {
			compactionIndex = index
			break
		}
	}
	if compactionIndex < 0 {
		return visibleEntries(path)
	}
	boundary := path[compactionIndex].Compaction.FirstKeptEntryID
	keptFrom := 0
	for index := 0; index < compactionIndex; index++ {
		if path[index].ID == boundary {
			keptFrom = index
			break
		}
	}
	context := make([]Entry, 0, len(path)-keptFrom+1)
	context = append(context, path[compactionIndex])
	context = append(context, visibleEntries(path[keptFrom:compactionIndex])...)
	context = append(context, visibleEntries(path[compactionIndex+1:])...)
	return context
}

// visibleEntries 执行 visibleEntries 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []Entry 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func visibleEntries(path []Entry) []Entry {
	return append(make([]Entry, 0, len(path)), path...)
}

// CompactionSummaryWrap 是压缩摘要进入上下文时的固定包裹文本，对齐 pi。
const CompactionSummaryWrap = "The conversation history before this point was compacted into the following summary:\n<summary>\n%s\n</summary>"

// ContextMessages 把上下文条目转换为发给模型的 message 列表。
// message 原样透传；compaction 投影为带包裹文本的 user 消息；其余类型
// 不产生消息。
// 参数 context 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []contract.Message 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func ContextMessages(context []Entry) []contract.Message {
	messages := make([]contract.Message, 0, len(context))
	for _, entry := range context {
		switch {
		case entry.Message != nil:
			messages = append(messages, *entry.Message)
		case entry.Compaction != nil:
			messages = append(messages, contract.Message{
				Role:    contract.RoleUser,
				Content: fmt.Sprintf(CompactionSummaryWrap, entry.Compaction.Summary),
			})
		}
	}
	return messages
}

// CurrentProfile 返回路径上最后一次 model_change 的 profile；没有切换记录
// 时返回空串，由调用方决定默认 profile。
// 参数 path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func CurrentProfile(path []Entry) string {
	for _, entry := range slices.Backward(path) {
		if entry.ModelChange != nil {
			return entry.ModelChange.Profile
		}
	}
	return ""
}

// SessionName 返回路径上最后一次 session_info 的名称。
// 参数 path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func SessionName(path []Entry) string {
	for _, entry := range slices.Backward(path) {
		if entry.SessionInfo != nil && entry.SessionInfo.Name != "" {
			return entry.SessionInfo.Name
		}
	}
	return ""
}

// SortedEntryIDs 返回按 id 排序的条目 id 列表，供测试与调试使用。
// 参数 entries 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func SortedEntryIDs(entries []Entry) []string {
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.ID)
	}
	sort.Strings(ids)
	return ids
}
