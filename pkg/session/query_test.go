package session

import (
	"errors"
	"os"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

func TestSessionListUsesCanonicalFilterValidation(t *testing.T) {
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	filter := ListFilter{State: "future"}
	if _, err := service.List(filter); err == nil {
		t.Fatal("Service accepted invalid Session list filter")
	} else if _, storeErr := service.store.list(filter); storeErr == nil {
		t.Fatal("Store accepted invalid Session list filter")
	} else if err.Error() != storeErr.Error() {
		t.Fatalf("Service error=%q Store error=%q", err, storeErr)
	}
	if _, err := service.List(ListFilter{}); err != nil {
		t.Fatalf("default Session list filter failed: %v", err)
	}
	if _, err := service.List(ListFilter{
		State: SessionArchived,
	}); err != nil {
		t.Fatalf("valid Session list filter failed: %v", err)
	}
}

func TestCreateWithIDPublishesExactIdentityOnce(t *testing.T) {
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	sessionID := "session_11111111111111111111111111111111"
	value, err := service.CreateWithID(sessionID, RetentionPinned)
	if err != nil {
		t.Fatal(err)
	}
	if value.ID != sessionID || value.Retention != RetentionPinned ||
		value.State != SessionIdle {
		t.Fatalf("Session = %#v", value)
	}
	if _, err := service.CreateWithID(sessionID, RetentionPinned); err == nil ||
		!errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create error = %v", err)
	}
}

func TestSessionCollectionQueriesRejectMissingSession(t *testing.T) {
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	sessionID := "session_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
	tests := []struct {
		name  string
		query func() error
	}{
		{
			name: "messages",
			query: func() error {
				_, err := service.Messages(sessionID, 0)
				return err
			},
		},
		{
			name: "events",
			query: func() error {
				_, err := service.Events(sessionID, 0)
				return err
			},
		},
		{
			name: "executions",
			query: func() error {
				_, err := service.Executions(sessionID)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.query(); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("query error=%v, want not-exist", err)
			}
		})
	}
}

func TestSubmitToolResultReturnsNotFoundForMissingTurnWithoutMutation(
	t *testing.T,
) {
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	value, err := service.Create(RetentionStandard)
	if err != nil {
		t.Fatal(err)
	}
	before, err := service.Get(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, runtimeErr := service.SubmitToolResult(
		value.ID,
		"turn_00000000000000000000000000000000",
		ToolResultInput{
			ToolCallID: "call_missing", IdempotencyKey: "missing-turn",
			Content: `{"ok":true}`,
		},
	)
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorNotFound {
		t.Fatalf("error=%v, want not_found", runtimeErr)
	}
	after, err := service.Get(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("missing turn mutated Session: before=%#v after=%#v", before, after)
	}
}

func TestDeleteMarksActiveAndBlockedSessionsAsConflict(t *testing.T) {
	for _, state := range []SessionState{SessionActive, SessionBlocked} {
		t.Run(string(state), func(t *testing.T) {
			service := newTestService(t, &scriptedGenerator{}, nil, nil)
			value, err := service.Create(RetentionStandard)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.store.withLock(value.ID, func() error {
				current, err := service.store.loadSession(value.ID)
				if err != nil {
					return err
				}
				current.State = state
				return service.store.writeSession(current)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := service.Delete(value.ID); err == nil ||
				!errors.Is(err, ErrConflict) {
				t.Fatalf("delete error=%v, want ErrConflict", err)
			}
			if _, err := service.Get(value.ID); err != nil {
				t.Fatalf("conflicting delete removed Session: %v", err)
			}
		})
	}
}
