package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/yy003x/sn-code/pkg/contract"
	"github.com/yy003x/sn-code/pkg/provider"
)

type Driver interface {
	ExecutionIdentity() DriverExecutionIdentity
	Validate(Profile) error
	Stream(
		context.Context,
		ResolvedModel,
		contract.ModelRequest,
		contract.EventSink,
	) (contract.ModelResult, provider.Attempt, *contract.RuntimeError)
}

type Generator interface {
	Generate(context.Context, contract.GenerateRequest) (contract.ModelResult, *contract.RuntimeError)
	GenerateStream(
		context.Context,
		contract.GenerateRequest,
		contract.EventSink,
	) (contract.ModelResult, *contract.RuntimeError)
}

// ManagedExecutionValidator is an optional Generator capability restriction.
// Agent consumers must check it before any model/tool effect. Wrappers around
// a restricted Generator must forward this capability rather than erase it.
type ManagedExecutionValidator interface {
	ValidateManagedExecution(profileID string) error
}

func (service *Service) ValidateManagedExecution(profileID string) error {
	if service == nil || service.catalog == nil {
		return fmt.Errorf("model service is unavailable")
	}
	profile, exists := service.catalog.Get(profileID)
	if !exists {
		return fmt.Errorf("unknown model profile %q", profileID)
	}
	return profile.ValidateManagedExecution()
}

type Service struct {
	catalog         *Catalog
	drivers         map[DriverName]Driver
	getenv          func(string) (string, bool)
	attemptObserver AttemptObserver
	callObserver    CallObserver
	credentials     CredentialResolver
}

type ServiceOptions struct {
	Getenv          func(string) (string, bool)
	AttemptObserver AttemptObserver
	CallObserver    CallObserver
	Credentials     CredentialResolver
}

func NewService(
	catalog *Catalog,
	drivers map[DriverName]Driver,
	options ServiceOptions,
) (*Service, error) {
	if catalog == nil {
		return nil, fmt.Errorf("model catalog is required")
	}
	if len(drivers) == 0 {
		return nil, fmt.Errorf("at least one model driver is required")
	}
	values := make(map[DriverName]Driver, len(drivers))
	for name, driver := range drivers {
		if driver == nil {
			return nil, fmt.Errorf("model driver %q is nil", name)
		}
		identity := driver.ExecutionIdentity()
		if err := identity.Validate(); err != nil {
			return nil, fmt.Errorf("model driver %q: %w", name, err)
		}
		if identity.Driver != name {
			return nil, fmt.Errorf(
				"model driver %q execution identity declares driver %q",
				name,
				identity.Driver,
			)
		}
		values[name] = driver
	}
	for _, id := range catalog.IDs() {
		profile, _ := catalog.Get(id)
		driver, exists := values[profile.Driver]
		if !exists {
			return nil, fmt.Errorf("model profile %q references unregistered driver %q", id, profile.Driver)
		}
		if err := driver.Validate(profile); err != nil {
			return nil, fmt.Errorf("model profile %q: %w", id, err)
		}
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	return &Service{
		catalog: catalog, drivers: values, getenv: getenv,
		attemptObserver: options.AttemptObserver,
		callObserver:    options.CallObserver,
		credentials:     options.Credentials,
	}, nil
}

// ExecutionSnapshot freezes the non-secret API Profile and the semantic
// identity of the concrete driver selected by this Service. Header ${VAR}
// references are kept unresolved so no secret value enters the snapshot.
func (service *Service) ExecutionSnapshot(
	profileID string,
) (ExecutionSnapshot, error) {
	if service == nil || service.catalog == nil {
		return ExecutionSnapshot{}, fmt.Errorf("model service is unavailable")
	}
	profile, exists := service.catalog.Get(profileID)
	if !exists {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model profile %q was not found", profileID,
		)
	}
	driver, exists := service.drivers[profile.Driver]
	if !exists || driver == nil {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model profile %q driver %q is unavailable",
			profileID,
			profile.Driver,
		)
	}
	identity := driver.ExecutionIdentity()
	if err := identity.Validate(); err != nil {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model profile %q driver identity: %w", profileID, err,
		)
	}
	if identity.Driver != profile.Driver {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model profile %q driver identity does not match Profile",
			profileID,
		)
	}
	return newExecutionSnapshot(profileID, profile, identity)
}

func (service *Service) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return service.GenerateStream(ctx, request, nil)
}

func (service *Service) GenerateStream(
	ctx context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (result contract.ModelResult, resultError *contract.RuntimeError) {
	if err := request.Validate(); err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest, err.Error(),
		)
	}
	profile, exists := service.catalog.Get(request.ModelProfile)
	if !exists {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInvalidRequest,
			contract.PhaseProfile,
			fmt.Sprintf("model profile %q was not found", request.ModelProfile),
		)
	}
	driver, exists := service.drivers[profile.Driver]
	if !exists {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInternal, contract.PhaseProfile, "model driver is unavailable",
		)
	}
	resolved, secrets, err := service.catalog.resolve(request.ModelProfile, service.getenv)
	if err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorAuthenticationFailed, contract.PhaseProfile, err.Error(),
		)
	}
	input, err := cloneValue(request.Input)
	if err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInternal, contract.PhaseRequest, "cannot clone model request",
		)
	}
	input.Options.ExtraBody = contract.MergeExtraBody(profile.Parameters.ExtraBody, input.Options.ExtraBody)
	if tokenLimit := profile.DefaultTokenLimit(); input.Options.MaxOutputTokens == nil &&
		tokenLimit != nil {
		value := *tokenLimit
		input.Options.MaxOutputTokens = &value
	}
	if input.Options.Temperature == nil && profile.Parameters.Temperature != nil {
		value := *profile.Parameters.Temperature
		input.Options.Temperature = &value
	}
	if input.Options.TopP == nil && profile.Parameters.TopP != nil {
		value := *profile.Parameters.TopP
		input.Options.TopP = &value
	}
	if len(input.Options.StopSequences) == 0 &&
		len(profile.Parameters.StopSequences) > 0 {
		input.Options.StopSequences = append(
			[]string(nil), profile.Parameters.StopSequences...,
		)
	}
	if input.Options.Stream != nil {
		value := *input.Options.Stream
		resolved.Parameters.Stream = &value
	}
	callContext, cancel := context.WithTimeout(ctx, time.Duration(resolved.Timeout))
	defer cancel()
	if err := profile.ValidateSingleCallInput(input); err != nil {
		return contract.ModelResult{}, runtimeError(contract.ErrorInvalidRequest, contract.PhaseRequest, err.Error())
	}
	authSecrets, err := service.resolveCredentials(callContext, &resolved)
	if err != nil {
		code := contract.ErrorAuthenticationFailed
		if callContext.Err() != nil {
			code = contract.ErrorCancelled
			if errors.Is(callContext.Err(), context.DeadlineExceeded) {
				code = contract.ErrorTimeout
			}
		}
		return contract.ModelResult{}, runtimeError(code, contract.PhaseProfile, err.Error())
	}
	secrets = append(secrets, authSecrets...)

	state := streamState{
		sink: sink, expected: 1,
		toolCalls: make(map[string]string),
	}
	result, wireAttempt, callError := driver.Stream(
		callContext, resolved, input, state.accept,
	)
	defer func() {
		if pending := pendingCallAttempts(ctx); pending != nil {
			pending.stashRedactedError(resultError, secrets)
		}
		service.observeAttempt(ctx, request.ModelProfile, wireAttempt, resultError, secrets)
	}()
	if state.failure != nil {
		return contract.ModelResult{}, state.failure
	}
	if state.sinkFailure != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorCancelled, contract.PhaseConsumer, "event sink stopped",
		)
	}
	if callError != nil {
		return contract.ModelResult{}, callError
	}
	if callContext.Err() != nil {
		code := contract.ErrorCancelled
		if errors.Is(callContext.Err(), context.DeadlineExceeded) {
			code = contract.ErrorTimeout
		}
		return contract.ModelResult{}, runtimeError(code, contract.PhaseProvider, callContext.Err().Error())
	}
	if err := result.Validate(); err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInvalidProviderResponse, contract.PhaseProvider, err.Error(),
		)
	}
	if !state.started {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			"driver did not emit model.started",
		)
	}
	if err := state.complete(result); err != nil {
		return contract.ModelResult{}, err
	}
	return result, nil
}

func notifyAttemptObserver(observer AttemptObserver, attempt Attempt) {
	defer func() { _ = recover() }()
	observer(attempt)
}

type streamState struct {
	sink             contract.EventSink
	expected         uint64
	started          bool
	pendingCompleted *contract.Event
	toolCalls        map[string]string
	failure          *contract.RuntimeError
	sinkFailure      error
}

func (state *streamState) accept(event contract.Event) error {
	if state.failure != nil || state.sinkFailure != nil {
		return fmt.Errorf("model stream is already failed")
	}
	// 事件面向消费者原样传递：resolved secret 只存在于 HTTP 请求层，
	// 不在事件面上做字节替换（替换本身会破坏 JSON 结构，见 redactValue 注释）。
	if event.Sequence != state.expected {
		state.failure = runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			fmt.Sprintf("event sequence=%d, want %d", event.Sequence, state.expected),
		)
		return state.failure
	}
	state.expected++
	if event.Sequence == 1 {
		if event.Type != contract.EventModelStarted {
			state.failure = runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				"first model event must be model.started",
			)
			return state.failure
		}
		state.started = true
	} else if event.Type == contract.EventModelStarted {
		state.failure = runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			"model.started can only be the first event",
		)
		return state.failure
	}
	if state.pendingCompleted != nil {
		state.failure = runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			"event follows model.completed",
		)
		return state.failure
	}
	if event.Type == contract.EventModelCompleted {
		if event.Model != nil &&
			(event.Model.Text != "" || event.Model.ToolCallID != "" ||
				event.Model.ToolCall != nil) {
			state.failure = runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				"model.completed contains an invalid payload",
			)
			return state.failure
		}
		state.pendingCompleted = &event
		return nil
	}
	if err := event.Validate(); err != nil {
		state.failure = runtimeError(
			contract.ErrorInvalidProviderResponse, contract.PhaseProvider, err.Error(),
		)
		return state.failure
	}
	switch event.Type {
	case contract.EventToolCallStarted:
		call := event.Model.ToolCall
		if _, exists := state.toolCalls[call.ID]; exists {
			state.failure = runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf("tool call id %q was started more than once", call.ID),
			)
			return state.failure
		}
		state.toolCalls[call.ID] = call.Name
	case contract.EventToolCallArgumentsDelta:
		if _, exists := state.toolCalls[event.Model.ToolCallID]; !exists {
			state.failure = runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf(
					"tool arguments delta references unknown call id %q",
					event.Model.ToolCallID,
				),
			)
			return state.failure
		}
	}
	return state.emit(event)
}

func (state *streamState) complete(result contract.ModelResult) *contract.RuntimeError {
	resultCalls := make(map[string]string, len(result.Message.ToolCalls))
	for _, call := range result.Message.ToolCalls {
		startedName, exists := state.toolCalls[call.ID]
		if !exists {
			return runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf("result contains tool call id %q without a start event", call.ID),
			)
		}
		if startedName != call.Name {
			return runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf("tool call id %q changed name", call.ID),
			)
		}
		resultCalls[call.ID] = call.Name
	}
	for id := range state.toolCalls {
		if _, exists := resultCalls[id]; !exists {
			return runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf("started tool call id %q is missing from the result", id),
			)
		}
	}
	var completed contract.Event
	if state.pendingCompleted == nil {
		completed = contract.Event{
			Sequence: state.expected,
			Type:     contract.EventModelCompleted,
			Model:    &contract.ModelEvent{Result: &result},
		}
		state.expected++
	} else {
		completed = *state.pendingCompleted
		if completed.Model == nil {
			completed.Model = &contract.ModelEvent{}
		}
		if completed.Model.Result != nil && !reflect.DeepEqual(*completed.Model.Result, result) {
			return runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				"model.completed result differs from driver result",
			)
		}
		completed.Model.Result = &result
	}
	if err := completed.Validate(); err != nil {
		return runtimeError(
			contract.ErrorInvalidProviderResponse, contract.PhaseProvider, err.Error(),
		)
	}
	if err := state.emit(completed); err != nil {
		if state.failure != nil {
			return state.failure
		}
		return runtimeError(
			contract.ErrorCancelled, contract.PhaseConsumer, "event sink stopped",
		)
	}
	return nil
}

func (state *streamState) emit(event contract.Event) error {
	if state.sink == nil {
		return nil
	}
	current, err := cloneValue(event)
	if err != nil {
		state.failure = runtimeError(
			contract.ErrorInternal, contract.PhaseConsumer, "cannot clone model event",
		)
		return state.failure
	}
	if err := state.sink(current); err != nil {
		state.sinkFailure = err
		return err
	}
	return nil
}

func runtimeError(
	code contract.ErrorCode,
	phase contract.ErrorPhase,
	message string,
) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: code, Phase: phase, Message: message, Retryable: false,
	}
}

// redactValue 深拷贝 value 并把 resolved secret 出现的字符串值替换为
// [REDACTED]。它只服务本地诊断面（execution log、attempt observer）：
// secret 只应存在于 HTTP 请求层，日志面收集前做最后兜底。
//
// 实现是手术式的：按 JSON 子树递归，只有包含 secret 的字符串值被重写，
// key 与结构永不改写；不含 secret 的子树保留原始字节（键序、空白、数字
// 字面量与转义形态不变），诊断副本与 wire 证据保持逐字节一致。此前对整段
// JSON 文本做字节替换的写法，在 secret 恰好是字段名子串（如单字符 key 命中
// "text"）时会破坏字段并导致下游校验失败。
func redactValue[T any](value T, secrets []string) (T, error) {
	data, err := json.Marshal(value)
	if err != nil {
		var zero T
		return zero, err
	}
	var result T
	if len(secrets) == 0 {
		if err := json.Unmarshal(data, &result); err != nil {
			var zero T
			return zero, err
		}
		return result, nil
	}
	// 长 secret 先替换，避免短 secret 截断命中位置后长 secret 失配。
	ordered := append([]string(nil), secrets...)
	sort.Slice(ordered, func(left, right int) bool {
		return len(ordered[left]) > len(ordered[right])
	})
	if err := json.Unmarshal(redactJSON(data, ordered), &result); err != nil {
		var zero T
		return zero, err
	}
	return result, nil
}

// redactJSON 返回替换 secret 之后的 JSON 文档。字符串叶子解码后做文本替换，
// 未命中保持原字节；数组与对象只在有子树变化时重编码（重编码会排序键，仅
// 影响含 secret 的子树）；数字、布尔、null 与对象 key 一律原样。
// 参数 data、secrets 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []byte 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func redactJSON(data []byte, secrets []string) []byte {
	if len(data) == 0 {
		return data
	}
	switch data[0] {
	case '"':
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return data
		}
		redacted := redactString(text, secrets)
		if redacted == text {
			return data
		}
		if encoded, err := encodeJSONCompact(redacted); err == nil {
			return encoded
		}
		return data
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return data
		}
		redactedItems := make([]json.RawMessage, len(items))
		changed := false
		for index, item := range items {
			redactedItems[index] = redactJSON(item, secrets)
			changed = changed || !bytes.Equal(redactedItems[index], item)
		}
		if !changed {
			return data
		}
		var buffer bytes.Buffer
		buffer.WriteByte('[')
		for index, item := range redactedItems {
			if index > 0 {
				buffer.WriteByte(',')
			}
			buffer.Write(item)
		}
		buffer.WriteByte(']')
		return buffer.Bytes()
	case '{':
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(data, &fields); err != nil {
			return data
		}
		redactedFields := make(map[string]json.RawMessage, len(fields))
		changed := false
		for key, raw := range fields {
			redactedFields[key] = redactJSON(raw, secrets)
			changed = changed || !bytes.Equal(redactedFields[key], raw)
		}
		if !changed {
			return data
		}
		if encoded, err := encodeJSONCompact(redactedFields); err == nil {
			return encoded
		}
		return data
	default:
		return data
	}
}

// encodeJSONCompact 产出紧凑且不做 HTML 转义的 JSON，贴近 wire 原始形态。
// 参数 value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []byte、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func encodeJSONCompact(value any) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buffer.Bytes(), []byte("\n")), nil
}

// redactString 执行 redactString 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 value、secrets 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func redactString(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func cloneValue[T any](value T) (T, error) {
	data, err := json.Marshal(value)
	if err != nil {
		var zero T
		return zero, err
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		var zero T
		return zero, err
	}
	return result, nil
}

func (resolved ResolvedModel) String() string {
	return fmt.Sprintf(
		"model=%s driver=%s endpoint=%s profile=%s headers=[REDACTED]",
		resolved.Model,
		resolved.Driver,
		resolved.Endpoint,
		resolved.ID,
	)
}
