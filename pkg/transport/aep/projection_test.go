package aep

import (
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/yy003x/sn-code/pkg/contract"
	"github.com/yy003x/sn-code/pkg/run"
)

func TestProjectRunStatusFollowsMappingTable(t *testing.T) {
	for _, test := range []struct {
		state           run.State
		cancelRequested bool
		want            string
	}{
		{run.StateQueued, false, statusPending},
		{run.StateRunning, false, statusRunning},
		{run.StatePaused, false, statusWaiting},
		{run.StateCompleted, false, statusSucceeded},
		{run.StateFailed, false, statusFailed},
		{run.StateCancelled, false, statusCancelled},
		{run.StateNeedsReconciliation, false, statusFailed},
		{run.StateRunning, true, statusCancelling},
		{run.StateQueued, true, statusCancelling},
		{run.StateCancelled, true, statusCancelled},
	} {
		got := projectRunStatus(run.Record{
			State: test.state, CancelRequested: test.cancelRequested,
		})
		if got != test.want {
			t.Fatalf("state=%s cancel=%t got=%s want=%s",
				test.state, test.cancelRequested, got, test.want)
		}
	}
}

func TestProjectRunErrorAddsReconciliationCode(t *testing.T) {
	if value := projectRunError(run.Record{State: run.StateCompleted}); value != nil {
		t.Fatalf("completed run error=%#v", value)
	}
	value := projectRunError(run.Record{State: run.StateNeedsReconciliation})
	if value == nil || value.Code != "reconciliation_required" {
		t.Fatalf("reconciliation error=%#v", value)
	}
	for _, code := range []contract.ErrorCode{contract.ErrorConflict, contract.ErrorInternal} {
		original := &contract.RuntimeError{
			Code: code, Phase: contract.PhaseRun, Message: "execution outcome unknown",
		}
		projected := projectRunError(run.Record{
			State: run.StateNeedsReconciliation, Error: original,
		})
		if projected == nil || projected.Code != "reconciliation_required" ||
			projected.Phase != string(original.Phase) || projected.Message != original.Message {
			t.Fatalf("reconciliation error=%#v original=%#v", projected, original)
		}
		if original.Code != code {
			t.Fatalf("original error changed: %#v", original)
		}
	}
	withError := projectRunError(run.Record{
		State: run.StateFailed,
		Error: &contract.RuntimeError{
			Code: contract.ErrorTimeout, Phase: contract.PhaseRun,
			Message: "deadline",
		},
	})
	if withError == nil || withError.Code != "timeout" ||
		withError.Phase != "run" || withError.Message != "deadline" {
		t.Fatalf("error=%#v", withError)
	}
}

func TestProjectEventMapsTerminalAndSettledEvents(t *testing.T) {
	timestamp := time.Date(2026, 10, 28, 7, 10, 0, 0, time.UTC)
	now := func() time.Time { return timestamp }
	for _, test := range []struct {
		name           string
		event          contract.Event
		wantType       string
		wantData       string
		wantNativeType string
	}{
		{
			"completed",
			contract.Event{Sequence: 3, Type: contract.EventRunCompleted, Time: &timestamp},
			"run.completed",
			`{"status":"succeeded"}`,
			"",
		},
		{
			"failed",
			contract.Event{
				Sequence: 4, Type: contract.EventRunFailed, Time: &timestamp,
				Error: &contract.RuntimeError{Code: contract.ErrorInternal},
			},
			"run.failed",
			`{"error":"internal","status":"failed"}`,
			"",
		},
		{
			"cancelled",
			contract.Event{Sequence: 5, Type: contract.EventRunCancelled, Time: &timestamp},
			"run.cancelled",
			`{"status":"cancelled"}`,
			"",
		},
		{
			"settled",
			contract.Event{
				Sequence: 6, Type: contract.EventRunSettled, Time: &timestamp,
				Run: &contract.RunEvent{RunID: "run_1", State: "completed"},
			},
			"run.status_changed",
			`{"settled":true,"status":"succeeded"}`,
			"",
		},
		{
			"message_delta",
			contract.Event{
				Sequence: 7, Type: contract.EventExecutorMessageDelta, Time: &timestamp,
				Executor: &contract.ExecutorEvent{
					NativeType: "content_block_delta", ID: "msg_1", Text: "he",
				},
			},
			"message.delta",
			`{"messageId":"msg_1","text":"he"}`,
			"content_block_delta",
		},
		{
			"message_completed",
			contract.Event{
				Sequence: 8, Type: contract.EventExecutorMessage, Time: &timestamp,
				Executor: &contract.ExecutorEvent{
					NativeType: "item.completed", Text: "done",
				},
			},
			"message.completed",
			`{"text":"done"}`,
			"item.completed",
		},
		{
			"tool_started",
			contract.Event{
				Sequence: 9, Type: contract.EventExecutorToolStarted, Time: &timestamp,
				Executor: &contract.ExecutorEvent{
					NativeType: "item.started", ID: "item_1",
					Name: "command_execution",
					Data: json.RawMessage(`{"command":"/bin/echo hi"}`),
				},
			},
			"tool.started",
			`{"input":{"command":"/bin/echo hi"},"name":"command_execution","toolCallId":"item_1"}`,
			"item.started",
		},
		{
			"tool_completed",
			contract.Event{
				Sequence: 10, Type: contract.EventExecutorToolCompleted, Time: &timestamp,
				Executor: &contract.ExecutorEvent{
					NativeType: "item.completed", ID: "item_1", Text: "hi",
					IsError: true, Data: json.RawMessage(`{"exit_code":1}`),
				},
			},
			"tool.completed",
			`{"detail":{"exit_code":1},"isError":true,"output":"hi","toolCallId":"item_1"}`,
			"item.completed",
		},
		{
			"executor_native_event",
			contract.Event{
				Sequence: 11, Type: contract.EventExecutorNative, Time: &timestamp,
				Executor: &contract.ExecutorEvent{
					NativeType: "thread.custom",
					Data:       json.RawMessage(`{"x":1}`),
				},
			},
			"executor.event",
			`{"data":{"x":1},"nativeType":"thread.custom"}`,
			"thread.custom",
		},
		{
			"unknown_falls_back",
			contract.Event{Sequence: 12, Type: contract.EventModelStarted, Time: &timestamp},
			"executor.event",
			`{"nativeType":"model.started"}`,
			"",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, ok := projectEvent(test.event, "run_1", "codex", now)
			if !ok {
				t.Fatal("event not projected")
			}
			if envelope.Protocol != ProtocolName || envelope.Version != ProtocolVersion {
				t.Fatalf("envelope=%#v", envelope)
			}
			if envelope.EventID != "run_1:"+
				strconv.FormatUint(test.event.Sequence, 10) {
				t.Fatalf("eventId=%q", envelope.EventID)
			}
			if envelope.Seq != test.event.Sequence {
				t.Fatalf("seq=%d", envelope.Seq)
			}
			if envelope.Type != test.wantType {
				t.Fatalf("type=%q want=%q", envelope.Type, test.wantType)
			}
			data, err := json.Marshal(envelope.Data)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != test.wantData {
				t.Fatalf("data=%s want=%s", data, test.wantData)
			}
			if envelope.Timestamp != "2026-10-28T07:10:00Z" {
				t.Fatalf("timestamp=%q", envelope.Timestamp)
			}
			if envelope.Source == nil || envelope.Source.Executor != "codex" {
				t.Fatalf("source=%#v", envelope.Source)
			}
			if test.wantNativeType != "" &&
				envelope.Source.NativeEventType != test.wantNativeType {
				t.Fatalf("nativeEventType=%q want=%q",
					envelope.Source.NativeEventType, test.wantNativeType)
			}
		})
	}
}
