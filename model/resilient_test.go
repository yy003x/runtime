package model

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/yy003x/runtime/contract"
)

type fakeOutcome struct {
	result   contract.ModelResult
	err      *contract.RuntimeError
	events   []contract.Event
	snapshot ExecutionSnapshot
	snapErr  error
}

type fakeResilientInner struct {
	outcomes     []fakeOutcome
	calls        int
	snapshotCall int
}

func (fake *fakeResilientInner) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return fake.GenerateStream(ctx, request, nil)
}

func (fake *fakeResilientInner) GenerateStream(
	ctx context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	outcome := fake.outcomes[fake.calls]
	fake.calls++
	for _, event := range outcome.events {
		if sink != nil {
			_ = sink(event)
		}
	}
	return outcome.result, outcome.err
}

func (fake *fakeResilientInner) ExecutionSnapshot(
	profileID string,
) (ExecutionSnapshot, error) {
	fake.snapshotCall++
	return fake.outcomes[0].snapshot, fake.outcomes[0].snapErr
}

func newTestResilientModel(
	t *testing.T,
	fake *fakeResilientInner,
	policy RetryPolicy,
) *ResilientModel {
	t.Helper()
	model := &ResilientModel{inner: fake, policy: policy.normalized()}
	model.backoff = func(int, time.Duration) time.Duration { return 0 }
	model.sleep = func(context.Context, time.Duration) bool { return true }
	return model
}

func retryableProviderError(message string) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code:      contract.ErrorProviderUnavailable,
		Phase:     contract.PhaseProvider,
		Message:   message,
		Retryable: true,
	}
}

func TestResilientModelRetriesUntilSuccess(t *testing.T) {
	started := contract.Event{Sequence: 1, Type: contract.EventModelStarted}
	completed := contract.Event{Sequence: 2, Type: contract.EventModelCompleted}
	success := contract.ModelResult{
		Message:      contract.Message{Role: contract.RoleAssistant, Content: "ok"},
		FinishReason: contract.FinishStop,
	}
	fake := &fakeResilientInner{outcomes: []fakeOutcome{
		{err: retryableProviderError("503"), events: []contract.Event{started}},
		{result: success, events: []contract.Event{started, completed}},
	}}
	var received []contract.Event
	sink := func(event contract.Event) error {
		received = append(received, event)
		return nil
	}
	model := newTestResilientModel(t, fake, RetryPolicy{MaxAttempts: 3})

	result, err := model.GenerateStream(context.Background(), contract.GenerateRequest{}, sink)
	if err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if result.Message.Content != "ok" {
		t.Fatalf("unexpected result content %q", result.Message.Content)
	}
	if fake.calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", fake.calls)
	}
	// The failed attempt leaked a model.started to the capture sink; only the
	// successful attempt's events must reach the real sink.
	if len(received) != 2 {
		t.Fatalf("expected 2 replayed events, got %d (%v)", len(received), received)
	}
	if received[0].Type != contract.EventModelStarted ||
		received[1].Type != contract.EventModelCompleted {
		t.Fatalf("unexpected replayed events: %v", received)
	}
}

func TestResilientModelNonRetryableErrorIsNotRetried(t *testing.T) {
	nonRetryable := &contract.RuntimeError{
		Code:    contract.ErrorInvalidRequest,
		Phase:   contract.PhaseRequest,
		Message: "bad request",
	}
	fake := &fakeResilientInner{outcomes: []fakeOutcome{{err: nonRetryable}}}
	model := newTestResilientModel(t, fake, RetryPolicy{MaxAttempts: 3})

	_, err := model.GenerateStream(context.Background(), contract.GenerateRequest{}, nil)
	if err == nil || err.Code != contract.ErrorInvalidRequest {
		t.Fatalf("expected non-retryable error returned unchanged, got %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("non-retryable error must not be retried, got %d calls", fake.calls)
	}
}

func TestResilientModelExhaustsAttempts(t *testing.T) {
	fake := &fakeResilientInner{outcomes: []fakeOutcome{
		{err: retryableProviderError("503")},
		{err: retryableProviderError("503")},
		{err: retryableProviderError("503")},
	}}
	model := newTestResilientModel(t, fake, RetryPolicy{MaxAttempts: 3})

	_, err := model.GenerateStream(context.Background(), contract.GenerateRequest{}, nil)
	if err == nil || err.Code != contract.ErrorProviderUnavailable {
		t.Fatalf("expected last retryable error, got %v", err)
	}
	if fake.calls != 3 {
		t.Fatalf("expected exactly MaxAttempts calls, got %d", fake.calls)
	}
}

func TestResilientModelContextCancelDuringBackoff(t *testing.T) {
	fake := &fakeResilientInner{outcomes: []fakeOutcome{
		{err: retryableProviderError("503")},
		{err: retryableProviderError("503")},
	}}
	model := &ResilientModel{inner: fake, policy: RetryPolicy{MaxAttempts: 3}.normalized()}
	model.backoff = func(int, time.Duration) time.Duration { return time.Second }
	model.sleep = func(ctx context.Context, delay time.Duration) bool {
		return ctx.Err() == nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := model.GenerateStream(ctx, contract.GenerateRequest{}, nil)
	if err == nil || err.Code != contract.ErrorCancelled {
		t.Fatalf("expected cancelled error when ctx is done during backoff, got %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("expected 1 attempt before backoff cancel, got %d", fake.calls)
	}
}

func TestResilientModelGenerateAlsoRetries(t *testing.T) {
	success := contract.ModelResult{
		Message:      contract.Message{Role: contract.RoleAssistant, Content: "done"},
		FinishReason: contract.FinishStop,
	}
	fake := &fakeResilientInner{outcomes: []fakeOutcome{
		{err: retryableProviderError("timeout")},
		{result: success},
	}}
	model := newTestResilientModel(t, fake, RetryPolicy{MaxAttempts: 3})

	result, err := model.Generate(context.Background(), contract.GenerateRequest{})
	if err != nil {
		t.Fatalf("expected Generate to retry to success, got %v", err)
	}
	if result.Message.Content != "done" {
		t.Fatalf("unexpected result content %q", result.Message.Content)
	}
	if fake.calls != 2 {
		t.Fatalf("expected 2 attempts from Generate, got %d", fake.calls)
	}
}

func TestResilientModelExecutionSnapshotPassthrough(t *testing.T) {
	expected := ExecutionSnapshot{ProfileID: "profile-id", SchemaVersion: ExecutionSnapshotSchemaVersion}
	fake := &fakeResilientInner{outcomes: []fakeOutcome{{snapshot: expected}}}
	model := newTestResilientModel(t, fake, RetryPolicy{MaxAttempts: 3})

	snapshot, err := model.ExecutionSnapshot("profile-id")
	if err != nil {
		t.Fatalf("unexpected snapshot error: %v", err)
	}
	if fake.snapshotCall != 1 {
		t.Fatalf("expected snapshot to forward to inner, got %d calls", fake.snapshotCall)
	}
	if !reflect.DeepEqual(snapshot, expected) {
		t.Fatalf("snapshot not forwarded unchanged: got %+v want %+v", snapshot, expected)
	}
}

func TestRetryPolicyNormalizedAppliesDefaults(t *testing.T) {
	policy := RetryPolicy{}.normalized()
	if policy.MaxAttempts != 3 {
		t.Fatalf("default MaxAttempts = %d, want 3", policy.MaxAttempts)
	}
	if policy.BaseDelay != 200*time.Millisecond {
		t.Fatalf("default BaseDelay = %v, want 200ms", policy.BaseDelay)
	}
	if policy.MaxDelay < policy.BaseDelay {
		t.Fatalf("MaxDelay %v must be >= BaseDelay %v", policy.MaxDelay, policy.BaseDelay)
	}
	// MaxAttempts of 1 disables retry entirely.
	disabled := RetryPolicy{MaxAttempts: 0}.normalized()
	if disabled.MaxAttempts != 3 {
		t.Fatalf("zero MaxAttempts should still normalize to default, got %d", disabled.MaxAttempts)
	}
}
