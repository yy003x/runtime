//go:build darwin || linux

package codingtui

import "strings"

// Editor 是单行输入编辑器：光标移动、删除、历史回溯。提交的内容进入
// 历史，上/下键在历史间移动，移动时暂存未提交的草稿。
type Editor struct {
	text []rune
	// cursor 是插入位置（rune 下标），取值 0..len(text)。
	cursor        int
	history       []string
	historyIndex  int
	draft         string
	maxHistoryLen int
}

// NewEditor 执行 NewEditor 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 *Editor 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewEditor() *Editor {
	return &Editor{maxHistoryLen: 200}
}

// Value 返回当前文本。
func (editor *Editor) Value() string {
	return string(editor.text)
}

// TextBeforeCursor 返回光标之前的文本（列宽定位用）。
func (editor *Editor) TextBeforeCursor() string {
	return string(editor.text[:editor.cursor])
}

// Cursor 返回光标的 rune 下标。
func (editor *Editor) Cursor() int {
	return editor.cursor
}

// SetValue 替换文本并把光标放到末尾。
// 参数 value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (editor *Editor) SetValue(value string) {
	editor.text = []rune(value)
	editor.cursor = len(editor.text)
}

// Insert 在光标处插入文本。
// 参数 text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (editor *Editor) Insert(text string) {
	runes := []rune(text)
	if len(runes) == 0 {
		return
	}
	updated := make([]rune, 0, len(editor.text)+len(runes))
	updated = append(updated, editor.text[:editor.cursor]...)
	updated = append(updated, runes...)
	updated = append(updated, editor.text[editor.cursor:]...)
	editor.text = updated
	editor.cursor += len(runes)
}

// Backspace 删除光标前一个字符。
func (editor *Editor) Backspace() {
	if editor.cursor == 0 {
		return
	}
	editor.text = append(editor.text[:editor.cursor-1], editor.text[editor.cursor:]...)
	editor.cursor--
}

// Delete 删除光标处字符。
func (editor *Editor) Delete() {
	if editor.cursor >= len(editor.text) {
		return
	}
	editor.text = append(editor.text[:editor.cursor], editor.text[editor.cursor+1:]...)
}

// Left / Right / Home / End 移动光标。
func (editor *Editor) Left() {
	if editor.cursor > 0 {
		editor.cursor--
	}
}

// Right 执行 Right 对应的包内处理，并遵循调用方提供的输入契约。
func (editor *Editor) Right() {
	if editor.cursor < len(editor.text) {
		editor.cursor++
	}
}

// Home 执行 Home 对应的包内处理，并遵循调用方提供的输入契约。
func (editor *Editor) Home() {
	editor.cursor = 0
}

// End 执行 End 对应的包内处理，并遵循调用方提供的输入契约。
func (editor *Editor) End() {
	editor.cursor = len(editor.text)
}

// HistoryUp 向更早的历史移动；首次离开草稿时保存草稿。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (editor *Editor) HistoryUp() bool {
	if len(editor.history) == 0 {
		return false
	}
	if editor.historyIndex == len(editor.history) {
		editor.draft = string(editor.text)
		editor.historyIndex--
		editor.loadHistory(editor.historyIndex)
		return true
	}
	if editor.historyIndex > 0 {
		editor.historyIndex--
		editor.loadHistory(editor.historyIndex)
		return true
	}
	return false
}

// HistoryDown 向更新的历史移动，越过最早项时恢复草稿。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (editor *Editor) HistoryDown() bool {
	if editor.historyIndex >= len(editor.history) {
		return false
	}
	editor.historyIndex++
	if editor.historyIndex == len(editor.history) {
		editor.SetValue(editor.draft)
		return true
	}
	editor.loadHistory(editor.historyIndex)
	return true
}

// loadHistory 执行 loadHistory 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 index 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (editor *Editor) loadHistory(index int) {
	editor.SetValue(editor.history[index])
}

// Submit 返回当前文本并清空编辑器；非空文本进入历史。
func (editor *Editor) Submit() string {
	value := string(editor.text)
	editor.text = editor.text[:0]
	editor.cursor = 0
	editor.historyIndex = len(editor.history)
	editor.draft = ""
	if strings.TrimSpace(value) != "" {
		editor.history = append(editor.history, value)
		if len(editor.history) > editor.maxHistoryLen {
			editor.history = editor.history[len(editor.history)-editor.maxHistoryLen:]
		}
		editor.historyIndex = len(editor.history)
	}
	return value
}

// Clear 清空编辑内容（Ctrl+C）。
func (editor *Editor) Clear() {
	editor.text = editor.text[:0]
	editor.cursor = 0
	editor.historyIndex = len(editor.history)
}
