// Package codingsessions 负责把 sn-code 交互会话持久化为 append-only
// JSONL 文件。目录布局：{root}/<cwd-slug>/<timestamp>_<id>.jsonl。
// 单写者由上层保证；本包只做文件读写与路径编码。
package codingsessions

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yy003x/sn-code/internal/domain/identity"
	"github.com/yy003x/sn-code/pkg/contract"
)

// Store 管理一个会话根目录（active home 下的 sncode/sessions）。
type Store struct {
	root string
}

// NewStore 返回根目录为 root 的会话存储；root 不存在时在使用时创建。
func NewStore(root string) *Store {
	return &Store{root: root}
}

// Session 是一个打开的会话文件：内存中的条目视图 + 追加写句柄。
type Session struct {
	store   *Store
	Header  Header
	entries []Entry
	LeafID  string
	path    string
	file    *os.File
	writer  *bufio.Writer
}

// OpenStoreFile 打开已有会话文件并载入全部条目。
// 参数 path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *Session、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (store *Store) OpenStoreFile(path string) (*Session, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read session file: %w", err)
	}
	var header *Header
	entries := make([]Entry, 0, 64)
	for line := range strings.SplitSeq(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lineHeader, lineEntry, err := DecodeLine([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
		}
		if lineHeader != nil {
			if header != nil {
				return nil, fmt.Errorf("%s: duplicate session header", filepath.Base(path))
			}
			header = lineHeader
			continue
		}
		if lineEntry != nil {
			entries = append(entries, *lineEntry)
		}
	}
	if header == nil {
		return nil, fmt.Errorf("%s: missing session header", filepath.Base(path))
	}
	expectedDir := filepath.Join(store.root, CWDSlug(header.CWD))
	if filepath.Clean(filepath.Dir(path)) != expectedDir {
		return nil, fmt.Errorf("%s: session working directory does not match its location", filepath.Base(path))
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open session file for append: %w", err)
	}
	session := &Session{
		store:   store,
		Header:  *header,
		entries: entries,
		path:    path,
		file:    file,
		writer:  bufio.NewWriter(file),
	}
	if len(entries) > 0 {
		session.LeafID = entries[len(entries)-1].ID
	}
	return session, nil
}

// Create 新建会话文件并写入 header。parentSession 非空时记录派生来源。
// 参数 now、cwd、parentSession 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *Session、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (store *Store) Create(now time.Time, cwd, parentSession string) (*Session, error) {
	id, err := identity.New("sn_code_session")
	if err != nil {
		return nil, err
	}
	header := Header{
		Type:          "session",
		Version:       Version,
		ID:            id,
		Timestamp:     now.UTC(),
		CWD:           cwd,
		ParentSession: parentSession,
	}
	dir := filepath.Join(store.root, CWDSlug(cwd))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session directory: %w", err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%s_%s.jsonl", now.UTC().Format("20060102T150405"), id))
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create session file: %w", err)
	}
	session := &Session{
		store:  store,
		Header: header,
		path:   path,
		file:   file,
		writer: bufio.NewWriter(file),
	}
	if err := session.writeLine(header); err != nil {
		file.Close()
		return nil, err
	}
	return session, nil
}

// Append 写入一条条目：ID/ParentID/Timestamp 由本方法赋值，条目内容
// 由调用方通过 payload 构造。写入失败时会话保持一致（不更新内存状态）。
// 参数 entry 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) Append(entry Entry) (Entry, error) {
	id, err := identity.New("sn_code_entry")
	if err != nil {
		return Entry{}, err
	}
	entry.ID = id
	entry.ParentID = session.LeafID
	entry.Timestamp = time.Now().UTC()
	if err := session.writeLine(entry); err != nil {
		return Entry{}, err
	}
	session.entries = append(session.entries, entry)
	session.LeafID = entry.ID
	return entry, nil
}

// AppendMessage 追加一条模型可见消息。
// 参数 message 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) AppendMessage(message contract.Message) (Entry, error) {
	return session.Append(Entry{
		Type:    EntryTypeMessage,
		Message: &message,
	})
}

// AppendModelChange 追加 profile 切换记录。
// 参数 profile 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) AppendModelChange(profile string) (Entry, error) {
	return session.Append(Entry{
		Type:        EntryTypeModelChange,
		ModelChange: &ModelChange{Profile: profile},
	})
}

// AppendCompaction 追加压缩记录。
// 参数 summary、firstKeptEntryID、tokensBefore 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) AppendCompaction(summary, firstKeptEntryID string, tokensBefore int64) (Entry, error) {
	return session.Append(Entry{
		Type: EntryTypeCompaction,
		Compaction: &Compaction{
			Summary:          summary,
			FirstKeptEntryID: firstKeptEntryID,
			TokensBefore:     tokensBefore,
		},
	})
}

// AppendSessionInfo 追加会话显示名。
// 参数 name 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) AppendSessionInfo(name string) (Entry, error) {
	return session.Append(Entry{
		Type:        EntryTypeSessionInfo,
		SessionInfo: &SessionInfo{Name: name},
	})
}

// AppendCustom 追加私有状态条目（不进入模型上下文）。
// 参数 customType、data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) AppendCustom(customType string, data json.RawMessage) (Entry, error) {
	return session.Append(Entry{
		Type:   EntryTypeCustom,
		Custom: &Custom{CustomType: customType, Data: data},
	})
}

// Entries 返回当前全部条目的拷贝。
// 返回 []Entry 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) Entries() []Entry {
	return append([]Entry(nil), session.entries...)
}

// Path 返回会话条目的当前分支。
// 参数 leafID 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) Path(leafID string) ([]Entry, error) {
	return Path(session.entries, leafID)
}

// FilePath 返回会话文件在磁盘上的路径。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) FilePath() string {
	return session.path
}

// Close 刷新并关闭追加句柄；幂等。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) Close() error {
	if session.file == nil {
		return nil
	}
	err := session.writer.Flush()
	closeErr := session.file.Close()
	session.file = nil
	if err != nil {
		return err
	}
	return closeErr
}

// writeLine 执行 writeLine 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *Session) writeLine(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode session line: %w", err)
	}
	if _, err := session.writer.Write(append(encoded, '\n')); err != nil {
		return fmt.Errorf("write session line: %w", err)
	}
	if err := session.writer.Flush(); err != nil {
		return fmt.Errorf("flush session line: %w", err)
	}
	return nil
}

// Summary 是 /resume 选择器需要的会话摘要。
type Summary struct {
	ID        string
	Name      string
	Timestamp time.Time
	CWD       string
	Path      string
	Entries   int
}

// List 返回指定 cwd 目录下的会话摘要，按时间倒序排列。Name 取最后一次
// session_info；没有命名时用首条用户消息的截断预览。
func (store *Store) List(cwd string) ([]Summary, error) {
	dir := filepath.Join(store.root, CWDSlug(cwd))
	matches, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		return nil, err
	}
	summaries := make([]Summary, 0, len(matches))
	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var header *Header
		var name, preview string
		entries := 0
		for line := range strings.SplitSeq(string(raw), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			lineHeader, lineEntry, err := DecodeLine([]byte(line))
			if err != nil {
				break
			}
			if lineHeader != nil {
				header = lineHeader
				continue
			}
			if lineEntry == nil {
				continue
			}
			entries++
			if lineEntry.SessionInfo != nil && lineEntry.SessionInfo.Name != "" {
				name = lineEntry.SessionInfo.Name
			}
			if preview == "" && lineEntry.Message != nil &&
				lineEntry.Message.Role == contract.RoleUser && lineEntry.Message.Content != "" {
				preview = truncateRunes(lineEntry.Message.Content, 60)
			}
		}
		if header == nil {
			continue
		}
		if header.CWD != cwd {
			continue
		}
		if name == "" {
			name = preview
		}
		summaries = append(summaries, Summary{
			ID:        header.ID,
			Name:      name,
			Timestamp: header.Timestamp,
			CWD:       header.CWD,
			Path:      path,
			Entries:   entries,
		})
	}
	sort.Slice(summaries, func(left, right int) bool {
		return summaries[left].Timestamp.After(summaries[right].Timestamp)
	})
	return summaries, nil
}

// truncateRunes 执行 truncateRunes 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 value、limit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func truncateRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit]) + "…"
}

// CWDSlug 使用小写十六进制编码工作目录；编码超过常见的 255 字节目录组件
// 上限时使用带独立前缀的 SHA-256 摘要。短路径布局不变，读取时仍校验 Header.CWD。
func CWDSlug(cwd string) string {
	if len(cwd) > 255/2 {
		return fmt.Sprintf("sha256-%x", sha256.Sum256([]byte(cwd)))
	}
	return hex.EncodeToString([]byte(cwd))
}
