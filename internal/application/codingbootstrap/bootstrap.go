// Package codingbootstrap 装配编程产品，独立于 Runtime 的 Session/Run 执行面。
package codingbootstrap

import (
	"path/filepath"

	"github.com/yy003x/sn-code/internal/application/codingagent"
	"github.com/yy003x/sn-code/internal/application/codingagent/tools"
	"github.com/yy003x/sn-code/internal/application/modelruntime"
	"github.com/yy003x/sn-code/internal/infrastructure/codingsessions"
	"github.com/yy003x/sn-code/internal/infrastructure/layout"
	"github.com/yy003x/sn-code/pkg/agent"
	"github.com/yy003x/sn-code/pkg/model"
	"github.com/yy003x/sn-code/pkg/profile"
)

// Services 是 sn-code 交互 TUI 的组装产物：裸 model.Service（不经
// ResilientModel，保持逐 delta 的真流式）、API Profile 视图、会话存储与
// 工具注册表。
type Services struct {
	Model    *model.Service
	Profiles codingagent.ProfileSource
	Registry *agent.Registry
	Policy   map[string]tools.ToolPolicy
	Sessions *codingsessions.Store
	// AgentDir 是 sncode 配置目录：全局 AGENTS.md 与偏好文件所在地。
	AgentDir string
}

// Load 从 active home 组装 sn-code 依赖。sn-code 的模型调用
// 是携带 Profile ID 的真实 API 执行，沿用执行日志观察者与凭据解析。
// 参数 paths 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *Services、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func Load(paths layout.Paths) (*Services, error) {
	core, err := modelruntime.Load(paths)
	if err != nil {
		return nil, err
	}
	registry, err := tools.Registry()
	if err != nil {
		return nil, err
	}
	return &Services{
		Model:    core.Models,
		Profiles: snCodeProfiles{catalog: core.Profiles},
		Registry: registry,
		Policy:   tools.Policy(),
		Sessions: codingsessions.NewStore(filepath.Join(paths.Home, "sncode", "sessions")),
		AgentDir: filepath.Join(paths.Home, "sncode"),
	}, nil
}

// snCodeProfiles 把 profile.Catalog 的 API Profile 适配为 codingagent 的
// 只读 ProfileSource；CLI Profile 不进入 sn-code。
type snCodeProfiles struct {
	catalog *profile.Catalog
}

// IDs 返回 API Profile ID。
func (source snCodeProfiles) IDs() []string {
	ids := make([]string, 0, 8)
	for _, entry := range source.catalog.Entries() {
		if entry.Kind == profile.KindModel {
			ids = append(ids, entry.ID)
		}
	}
	return ids
}

// Get 返回指定 API Profile 的模型与上下文预算摘要；不存在或不是 API Profile 时返回 false。
func (source snCodeProfiles) Get(id string) (codingagent.ProfileInfo, bool) {
	entry, ok := source.catalog.Resolve(id)
	if !ok || entry.Kind != profile.KindModel || entry.Model == nil {
		return codingagent.ProfileInfo{}, false
	}
	window, reserved, _, _ := entry.Model.EffectiveContextBudgetForRequest(nil)
	return codingagent.ProfileInfo{
		ID:            entry.ID,
		Driver:        string(entry.Model.Driver),
		Model:         entry.Model.Model,
		ContextWindow: window,
		ReservedOut:   reserved,
	}, true
}

// 编译期断言：适配器满足编排层的 ProfileSource；裸 Service 满足最小模型
// 接口。
var (
	_ codingagent.ProfileSource = snCodeProfiles{}
	_ codingagent.Model         = (*model.Service)(nil)
)
