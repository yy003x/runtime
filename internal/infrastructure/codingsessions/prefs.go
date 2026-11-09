package codingsessions

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Preferences 保存 sn-code 的轻量本地偏好（当前只有默认 profile）。
// 文件不存在时返回零值，不视为错误。
type Preferences struct {
	LastProfile string `json:"last_profile,omitempty"`
}

// LoadPreferences 读取偏好文件；文件缺失返回零值。
// 参数 path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Preferences、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func LoadPreferences(path string) (Preferences, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Preferences{}, nil
		}
		return Preferences{}, fmt.Errorf("read preferences: %w", err)
	}
	var preferences Preferences
	if err := json.Unmarshal(raw, &preferences); err != nil {
		return Preferences{}, fmt.Errorf("decode preferences: %w", err)
	}
	return preferences, nil
}

// SavePreferences 原子写入偏好文件（临时文件 + rename）。
// 参数 path、preferences 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func SavePreferences(path string, preferences Preferences) error {
	encoded, err := json.MarshalIndent(preferences, "", "  ")
	if err != nil {
		return fmt.Errorf("encode preferences: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create preferences directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".preferences-*")
	if err != nil {
		return fmt.Errorf("create preferences temporary file: %w", err)
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(append(encoded, '\n')); err != nil {
		temporary.Close()
		return fmt.Errorf("write preferences: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return fmt.Errorf("replace preferences: %w", err)
	}
	return nil
}
