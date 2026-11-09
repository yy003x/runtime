//go:build darwin || linux

// Package codingtui 实现 sn-code 的交互终端界面：inline（非备用屏）布局，
// 已提交历史只打印一次，活动区（流式输出、编辑器、footer）整帧重绘。
// 不引入 TUI 框架；终端原始输入复用 terminalinput。
package codingtui

import "strings"

// runeWidth 估算一个 rune 的显示列宽。ASCII/半角为 1；常用 CJK 全角
// 区段按 2 估算。这是近似实现：目标是终端换行计行正确，而不是精确字形
// 度量。
// 参数 r 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 int 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func runeWidth(r rune) int {
	switch {
	case r == 0:
		return 0
	case r < 32 || (r >= 0x7f && r < 0xa0):
		return 0
	case r >= 0x1100 && r <= 0x115f, // Hangul Jamo
		r >= 0x2e80 && r <= 0xa4cf, // CJK 部首、假名、注音
		r >= 0xac00 && r <= 0xd7a3, // Hangul 音节
		r >= 0xf900 && r <= 0xfaff, // CJK 兼容表意
		r >= 0xfe30 && r <= 0xfe6f, // CJK 兼容形式
		r >= 0xff00 && r <= 0xff60, // 全角形式
		r >= 0xffe0 && r <= 0xffe6,
		r >= 0x1f300 && r <= 0x1f64f, // emoji 常用区段
		r >= 0x1f900 && r <= 0x1f9ff,
		r >= 0x20000 && r <= 0x3fffd: // CJK 扩展
		return 2
	default:
		return 1
	}
}

// stringWidth 返回字符串的显示列宽（忽略 ANSI 转义序列）。
// 参数 text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 int 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func stringWidth(text string) int {
	width := 0
	inEscape := false
	for _, r := range text {
		if inEscape {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEscape = false
			}
			continue
		}
		if r == 0x1b {
			inEscape = true
			continue
		}
		width += runeWidth(r)
	}
	return width
}

// wrapLines 把文本按显示宽度换行；保留显式换行，空行产出空串。
// 超宽的单行在宽度边界折行，CJK 全角字符不拆分。
// 参数 text、width 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func wrapLines(text string, width int) []string {
	if width < 1 {
		width = 1
	}
	var output []string
	for paragraph := range strings.SplitSeq(text, "\n") {
		if paragraph == "" {
			output = append(output, "")
			continue
		}
		lineWidth := 0
		var line strings.Builder
		for _, r := range paragraph {
			runeW := runeWidth(r)
			if lineWidth+runeW > width {
				output = append(output, line.String())
				line.Reset()
				lineWidth = 0
			}
			line.WriteRune(r)
			lineWidth += runeW
		}
		if line.Len() > 0 {
			output = append(output, line.String())
		}
	}
	return output
}

// truncateToWidth 截断字符串到显示宽度，超出时以 suffix 结尾。
// 参数 text、width、suffix 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func truncateToWidth(text string, width int, suffix string) string {
	if stringWidth(text) <= width {
		return text
	}
	target := max(width-stringWidth(suffix), 0)
	used := 0
	var kept strings.Builder
	for _, r := range text {
		runeW := runeWidth(r)
		if used+runeW > target {
			break
		}
		kept.WriteRune(r)
		used += runeW
	}
	return kept.String() + suffix
}
