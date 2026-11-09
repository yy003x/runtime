// Package modelruntime 装配共享模型能力，不加载产品会话、Run 或工具。
package modelruntime

import (
	"fmt"
	"time"

	"github.com/yy003x/sn-code/internal/application/authruntime"
	"github.com/yy003x/sn-code/internal/domain/identity"
	"github.com/yy003x/sn-code/internal/infrastructure/codexauth"
	"github.com/yy003x/sn-code/internal/infrastructure/executionlog"
	"github.com/yy003x/sn-code/internal/infrastructure/layout"
	"github.com/yy003x/sn-code/pkg/model"
	"github.com/yy003x/sn-code/pkg/profile"
	"github.com/yy003x/sn-code/pkg/provider/anthropic"
	"github.com/yy003x/sn-code/pkg/provider/openai"
	"github.com/yy003x/sn-code/pkg/provider/openairesponses"
)

// Services 只包含 Profile 和裸模型服务，重试由产品装配层选择。
type Services struct {
	Profiles *profile.Catalog
	Models   *model.Service
}

func Load(
	paths layout.Paths,
	reservedProfileIDs ...string,
) (*Services, error) {
	profiles, err := profile.Load(paths.ConfigDir, reservedProfileIDs...)
	if err != nil {
		return nil, err
	}
	models, err := model.NewService(
		profiles.ModelCatalog(),
		map[model.DriverName]model.Driver{
			model.DriverOpenAIChatCompletions: openai.New(nil),
			model.DriverAnthropicMessages:     anthropic.New(nil),
			model.DriverOpenAIResponses:       openairesponses.New(nil),
			model.DriverOpenAICodexResponses:  openairesponses.NewCodex(nil),
		},
		model.ServiceOptions{CallObserver: executionCallObserver(paths.LogsDir), Credentials: Auth(paths).Resolve},
	)
	if err != nil {
		return nil, fmt.Errorf("build model service: %w", err)
	}
	return &Services{Profiles: profiles, Models: models}, nil
}

func Auth(paths layout.Paths) *authruntime.Service {
	return authruntime.New(codexauth.Store{StateDir: paths.StateDir}, codexauth.NewClient(nil))
}

func executionCallObserver(logsDir string) model.CallObserver {
	return func(call model.CallObservation) {
		if len(call.Attempts) == 0 {
			return
		}
		callID, err := identity.New("call")
		if err != nil {
			return
		}
		attempts := make([]executionlog.APIAttempt, len(call.Attempts))
		for index, attempt := range call.Attempts {
			attempts[index] = executionlog.APIAttempt{
				Attempt: index + 1, Request: attempt.Wire.Request,
				Response: attempt.Wire.Response, Error: attempt.Error,
			}
		}
		_ = executionlog.AppendAPI(logsDir, executionlog.APIRecord{
			Time:      time.Now(),
			Namespace: executionlog.Namespace(call.Origin.Namespace),
			Profile:   call.ProfileID,
			Source:    call.Origin.Source,
			CallID:    callID,
			Attempts:  attempts,
			Error:     call.Error,
		})
	}
}
