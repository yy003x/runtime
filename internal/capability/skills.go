package capability

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Skill struct {
	Name           string         `json:"name" yaml:"name"`
	Description    string         `json:"description" yaml:"description"`
	Keywords       []string       `json:"keywords" yaml:"keywords"`
	Path           string         `json:"path" yaml:"-"`
	Root           string         `json:"-" yaml:"-"`
	Entry          string         `json:"entry" yaml:"entry"`
	DefaultProfile string         `json:"default_profile" yaml:"default_profile"`
	Profile        string         `json:"-" yaml:"profile"`
	PromptTemplate string         `json:"-" yaml:"prompt_template"`
	Raw            map[string]any `json:"-" yaml:"-"`
}

type SkillManager struct {
	skills map[string]Skill
	errors []map[string]string
}

func NewSkillManager() *SkillManager {
	return &SkillManager{skills: make(map[string]Skill)}
}

func (m *SkillManager) RegisterDir(path string) {
	info, err := os.Stat(path)
	if err != nil {
		m.errors = append(m.errors, map[string]string{"path": path, "error": "技能目录不存在"})
		return
	}
	var paths []string
	if !info.IsDir() {
		paths = []string{path}
	} else {
		first, _ := filepath.Glob(filepath.Join(path, "*.skill.yaml"))
		second, _ := filepath.Glob(filepath.Join(path, "*", "skill.yaml"))
		paths = append(first, second...)
		sort.Strings(paths)
	}
	for _, skillPath := range paths {
		data, readErr := os.ReadFile(skillPath)
		if readErr != nil {
			m.errors = append(m.errors, map[string]string{"path": skillPath, "error": readErr.Error()})
			continue
		}
		var skill Skill
		var raw map[string]any
		if err := yaml.Unmarshal(data, &skill); err != nil {
			m.errors = append(m.errors, map[string]string{"path": skillPath, "error": err.Error()})
			continue
		}
		_ = yaml.Unmarshal(data, &raw)
		if skill.Name == "" || skill.Description == "" {
			m.errors = append(m.errors, map[string]string{"path": skillPath, "error": "技能必须含 name 与 description"})
			continue
		}
		skill.Path, skill.Root, skill.Raw = skillPath, filepath.Dir(skillPath), raw
		if skill.DefaultProfile == "" {
			skill.DefaultProfile = skill.Profile
		}
		m.skills[skill.Name] = skill
	}
}

func (m *SkillManager) List() []Skill {
	names := make([]string, 0, len(m.skills))
	for name := range m.skills {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Skill, 0, len(names))
	for _, name := range names {
		out = append(out, m.skills[name])
	}
	return out
}

func (m *SkillManager) Get(name string) (Skill, error) {
	skill, ok := m.skills[name]
	if !ok {
		return Skill{}, fmt.Errorf("未加载 skill: %s", name)
	}
	return skill, nil
}

func (m *SkillManager) Route(query string) (Skill, bool) {
	lower := strings.ToLower(query)
	for _, skill := range m.List() {
		for _, keyword := range skill.Keywords {
			if strings.Contains(lower, strings.ToLower(keyword)) {
				return skill, true
			}
		}
	}
	return Skill{}, false
}

func (m *SkillManager) Doctor() map[string]any {
	return map[string]any{"ok": true, "loaded": len(m.skills), "errors": m.errors}
}

func (s Skill) Render(input, query string, variables map[string]any) (string, error) {
	template := s.PromptTemplate
	if template == "" && s.Entry != "" {
		data, err := os.ReadFile(filepath.Join(s.Root, s.Entry))
		if err != nil {
			return "", fmt.Errorf("skill %s: entry 不存在 %s", s.Name, s.Entry)
		}
		template = string(data)
	}
	if template == "" {
		return "", fmt.Errorf("skill %s: 缺少 prompt_template 或 entry", s.Name)
	}
	out := strings.ReplaceAll(strings.ReplaceAll(template, "{{input}}", input), "{{query}}", query)
	for key, value := range variables {
		out = strings.ReplaceAll(out, "{{"+key+"}}", fmt.Sprint(value))
	}
	if !strings.Contains(template, "{{input}}") && input != "" {
		out = strings.TrimRight(out, "\n") + "\n\n用户输入:\n" + input + "\n"
	}
	if !strings.Contains(template, "{{query}}") && query != "" {
		out = strings.TrimRight(out, "\n") + "\n\n用户请求:\n" + query + "\n"
	}
	return out, nil
}
