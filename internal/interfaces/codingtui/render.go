//go:build darwin || linux

package codingtui

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/yy003x/sn-code/internal/application/codingagent"
)

// ANSI 转义：隐藏/显示光标、清屏、清除到末尾、光标上移/列移。
const (
	// hideCursor 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	hideCursor = "\x1b[?25l"
	// showCursor 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	showCursor = "\x1b[?25h"
	// clearScreen 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	clearScreen = "\x1b[2J\x1b[3J\x1b[H"
	// eraseBelow 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	eraseBelow = "\x1b[J"
	// eraseLine 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	eraseLine = "\x1b[2K"
)

// dim/bold/color 输出助记。
const (
	// styleDim 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	styleDim = "\x1b[2m"
	// styleBold 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	styleBold = "\x1b[1m"
	// styleWarn 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	styleWarn = "\x1b[33m"
	// styleError 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	styleError = "\x1b[31m"
	// styleAccent 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	styleAccent = "\x1b[36m"
	// styleReset 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	styleReset = "\x1b[0m"
)

// streamingTailLines 限制活动区流式文本的显示行数：只保留尾部。
const streamingTailLines = 14

// toolLineView 是活动区中一条进行中的工具调用。
type toolLineView struct {
	ID   string
	Text string
}

// frame 描述一次要绘制的活动区：从上到下的行 + 编辑器光标位置。
type frame struct {
	lines []string
	// cursorLine/cursorCol 是光标在 frame 内的位置（行下标、显示列）；
	// cursorLine < 0 表示隐藏光标（如流式输出期间）。
	cursorLine int
	cursorCol  int
}

// viewState 是渲染一帧所需的全部 UI 状态快照。
type viewState struct {
	width  int
	busy   bool
	status string // 编辑器上方的临时状态行（空串不显示）

	streamTail   []string
	streamMore   bool // 尾部之外还有更早的内容被折叠
	activeTools  []toolLineView
	editor       *Editor
	editorHidden bool // 模态打开时不显示编辑器

	confirm *codingagent.ToolCallInfoForGate
	picker  *pickerState
	footer  string
}

// buildFrame 把视图状态组装为活动区行。顺序：状态行 → 流式尾部 → 进行中
// 工具 → 模态（确认/选择器）或编辑器 → footer。
// 参数 view 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 frame 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func buildFrame(view viewState) frame {
	var lines []string

	if view.status != "" {
		lines = append(lines, truncateToWidth(view.status, view.width, "…"))
	}
	lines = append(lines, view.streamTail...)
	if view.streamMore {
		lines = append(lines, styleDim+"…"+styleReset)
	}
	for _, tool := range view.activeTools {
		lines = append(lines, truncateToWidth(tool.Text, view.width, "…"))
	}

	cursorLine, cursorCol := -1, 0
	switch {
	case view.confirm != nil:
		lines = append(lines, confirmLines(view.confirm, view.width)...)
	case view.picker != nil:
		lines = append(lines, view.picker.lines(view.width)...)
	case !view.editorHidden:
		prompt := "> "
		if view.busy {
			prompt = styleAccent + "…" + styleReset + " "
		}
		editorLine := prompt + view.editor.Value()
		cursorLine = len(lines)
		cursorCol = stringWidth(prompt) + stringWidth(view.editor.TextBeforeCursor())
		lines = append(lines, truncateToWidth(editorLine, view.width, "…"))
	}
	if view.footer != "" {
		lines = append(lines, styleDim+truncateToWidth(view.footer, view.width, "…")+styleReset)
	}
	return frame{lines: lines, cursorLine: cursorLine, cursorCol: cursorCol}
}

// confirmLines 执行 confirmLines 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 call、width 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func confirmLines(call *codingagent.ToolCallInfoForGate, width int) []string {
	header := fmt.Sprintf("%s%s wants to run (effect=%s risk=%s)%s",
		styleWarn, call.Name, call.Effect, call.Risk, styleReset)
	return []string{
		header,
		truncateToWidth("  "+summarizeArguments(call.Name, call.Arguments), width, "…"),
		styleDim + "[y] allow once   [a] always allow   [n] deny" + styleReset,
	}
}

// pickerState 是 /model、/resume 共用的列表选择器。
type pickerState struct {
	title    string
	options  []pickerOption
	selected int
	onPick   func(pickerOption) error
}

type pickerOption struct {
	ID     string
	Label  string
	Detail string
}

// lines 执行 lines 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 width 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (picker *pickerState) lines(width int) []string {
	lines := []string{styleBold + picker.title + styleReset}
	for index, option := range picker.options {
		marker := "  "
		text := option.Label
		if option.Detail != "" {
			text += " " + styleDim + option.Detail + styleReset
		}
		if index == picker.selected {
			marker = styleAccent + "› " + styleReset
			text = styleAccent + option.Label + styleReset +
				" " + styleDim + option.Detail + styleReset
		}
		lines = append(lines, truncateToWidth(marker+text, width, "…"))
	}
	lines = append(lines, styleDim+"↑/↓ select · enter confirm · esc cancel"+styleReset)
	return lines
}

// move 执行 move 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 delta 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (picker *pickerState) move(delta int) {
	if len(picker.options) == 0 {
		return
	}
	picker.selected = (picker.selected + delta + len(picker.options)) % len(picker.options)
}

// summarizeArguments 提炼工具参数为单行摘要（确认门与工具行共用）。
// 参数 name、arguments 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func summarizeArguments(name string, arguments json.RawMessage) string {
	var fields map[string]any
	if err := json.Unmarshal(arguments, &fields); err != nil {
		return name
	}
	switch name {
	case "bash":
		if command, ok := fields["command"].(string); ok {
			return "$ " + strings.ReplaceAll(command, "\n", " ⏎ ")
		}
	case "read", "write", "ls", "grep", "find":
		if path, ok := fields["path"].(string); ok && path != "" {
			return path
		}
	case "edit":
		if path, ok := fields["path"].(string); ok {
			return path
		}
	}
	pairs := make([]string, 0, len(fields))
	for key, value := range fields {
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s=%s", key, encoded))
	}
	return name + " " + strings.Join(pairs, " ")
}

// painter 负责活动区的就地重绘：清除上一帧、打印当前帧、定位光标。
// 历史区由调用方直接追加打印，painter 不感知。
type painter struct {
	out       io.Writer
	lastLines int
}

// repaint 执行 repaint 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 built 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (p *painter) repaint(built frame) error {
	var builder strings.Builder
	if p.lastLines > 1 {
		fmt.Fprintf(&builder, "\x1b[%dA", p.lastLines-1)
	}
	builder.WriteString("\r" + eraseBelow)
	for index, line := range built.lines {
		if index > 0 {
			builder.WriteByte('\n')
		}
		if line == "" {
			continue
		}
		builder.WriteString(line)
	}
	p.lastLines = len(built.lines)
	// 光标定位：目标行相对末行的上移量；隐藏光标时停在帧尾。
	if built.cursorLine >= 0 {
		up := len(built.lines) - 1 - built.cursorLine
		if up > 0 {
			fmt.Fprintf(&builder, "\x1b[%dA", up)
		}
		builder.WriteString("\r")
		if built.cursorCol > 0 {
			fmt.Fprintf(&builder, "\x1b[%dC", built.cursorCol)
		}
	}
	_, err := io.WriteString(p.out, builder.String())
	return err
}

// scrollbreak 在历史区追加行后调用：活动区已被推到新的滚动位置，重置
// painter 的行计数。
func (p *painter) reset() {
	p.lastLines = 0
}
