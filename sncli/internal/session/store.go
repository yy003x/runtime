package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type Session struct {
	ID        string    `json:"id"`
	Provider  string    `json:"provider"`
	CWD       string    `json:"cwd"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Message struct {
	At       time.Time `json:"at"`
	Role     string    `json:"role"`
	Provider string    `json:"provider,omitempty"`
	RunID    string    `json:"run_id,omitempty"`
	Text     string    `json:"text"`
}

type Store struct {
	Root string
}

func (s Store) New(provider, cwd string) (*Session, error) {
	now := time.Now()
	session := &Session{
		ID:        newID(),
		Provider:  provider,
		CWD:       cwd,
		CreatedAt: now,
		UpdatedAt: now,
	}
	return session, s.Save(session)
}

func (s Store) Save(session *Session) error {
	session.UpdatedAt = time.Now()
	dir := s.dir(session.ID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "session.json"), data, 0644)
}

func (s Store) Load(id string) (*Session, error) {
	data, err := os.ReadFile(filepath.Join(s.dir(id), "session.json"))
	if os.IsNotExist(err) {
		data, err = os.ReadFile(filepath.Join(s.Root, id, "session.json"))
	}
	if err != nil {
		return nil, err
	}
	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, err
	}
	return &session, nil
}

func (s Store) Append(id string, msg Message) error {
	dir := s.dir(id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	msg.At = time.Now()
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "messages.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(data, '\n'))
	return err
}

func (s Store) List() ([]Session, error) {
	var out []Session
	err := filepath.WalkDir(s.Root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Name() != "session.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var session Session
		if err := json.Unmarshal(data, &session); err == nil {
			out = append(out, session)
		}
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[j].UpdatedAt.Before(out[i].UpdatedAt)
	})
	return out, nil
}

func (s Store) dir(id string) string {
	return filepath.Join(s.Root, datePartition(id), id)
}

func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func datePartition(id string) string {
	stamp := strings.Split(id, "-")[0]
	if len(stamp) >= 8 {
		date := stamp[:8]
		return date[:4] + "-" + date[4:6] + "-" + date[6:8]
	}
	return time.Now().UTC().Format("2006-01-02")
}
