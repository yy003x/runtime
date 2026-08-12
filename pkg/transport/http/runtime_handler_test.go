package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/agent"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/profile"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
	sqlitestore "github.com/yy003x/runtime/pkg/store/sqlite"
)

func TestRuntimeHandlerSessionAndAgentShareServices(t *testing.T) {
	handler, services := newRuntimeHandlerTest(t)
	created := performJSON(
		t, handler, http.MethodPost, "/v1/sessions", `{}`,
	)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var sessionValue session.Session
	if err := json.Unmarshal(created.Body.Bytes(), &sessionValue); err != nil {
		t.Fatal(err)
	}
	turn := performJSON(
		t, handler, http.MethodPost,
		"/v1/sessions/"+sessionValue.ID+"/turns",
		`{"profile_id":"api","input":"hello"}`,
	)
	if turn.Code != http.StatusOK ||
		!strings.Contains(turn.Body.String(), `"state":"completed"`) {
		t.Fatalf("turn status=%d body=%s", turn.Code, turn.Body.String())
	}
	var turnResult session.RunResult
	if err := json.Unmarshal(turn.Body.Bytes(), &turnResult); err != nil {
		t.Fatal(err)
	}
	executions := performJSON(
		t, handler, http.MethodGet,
		"/v1/sessions/"+sessionValue.ID+"/executions", "",
	)
	if executions.Code != http.StatusOK ||
		!strings.Contains(executions.Body.String(), turnResult.ExecutionID) {
		t.Fatalf(
			"executions status=%d body=%s",
			executions.Code, executions.Body.String(),
		)
	}
	execution := performJSON(
		t, handler, http.MethodGet,
		"/v1/sessions/"+sessionValue.ID+"/executions/"+turnResult.ExecutionID,
		"",
	)
	if execution.Code != http.StatusOK ||
		!strings.Contains(execution.Body.String(), `"state":"settled"`) {
		t.Fatalf(
			"execution status=%d body=%s",
			execution.Code, execution.Body.String(),
		)
	}
	agentResponse := performJSON(
		t, handler, http.MethodPost, "/v1/agent/run",
		`{"profile_id":"api","input":"finish"}`,
	)
	if agentResponse.Code != http.StatusOK {
		t.Fatalf(
			"agent status=%d body=%s",
			agentResponse.Code, agentResponse.Body.String(),
		)
	}
	var record runtime.Record
	if err := json.Unmarshal(agentResponse.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.State != runtime.StateCompleted || record.SettledSequence == 0 {
		t.Fatalf("record=%#v", record)
	}
	streamRequest := httptest.NewRequest(
		http.MethodGet, "/v1/runs/"+record.ID+"/events", nil,
	)
	streamRequest.Header.Set("Accept", "text/event-stream")
	streamRequest.Header.Set("Last-Event-ID", "1")
	stream := httptest.NewRecorder()
	handler.ServeHTTP(stream, streamRequest)
	if stream.Code != http.StatusOK ||
		!strings.Contains(stream.Body.String(), "event: run.settled") {
		t.Fatalf("stream status=%d body=%s", stream.Code, stream.Body.String())
	}
	values, err := services.List(context.Background(), runtime.ListFilter{})
	if err != nil || len(values) != 1 {
		t.Fatalf("runs=%#v error=%v", values, err)
	}
}

func TestRuntimeHandlerRejectsCommandProfileForAgentBeforeRunCreation(t *testing.T) {
	handler, services := newRuntimeHandlerTest(t)
	response := performJSON(
		t, handler, http.MethodPost, "/v1/agent/run",
		`{"profile_id":"cli","input":"must fail"}`,
	)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "requires an API model profile") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	values, err := services.List(context.Background(), runtime.ListFilter{})
	if err != nil || len(values) != 0 {
		t.Fatalf("invalid Agent mutated Run Store: runs=%#v error=%v", values, err)
	}
}

func TestRuntimeHandlerRejectsUnknownRunFields(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	response := performJSON(
		t, handler, http.MethodPost, "/v1/runs",
		`{"kind":"agent","profile_id":"api","input":"x","command":"/bin/sh"}`,
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestRuntimeHandlerObjectDTOsRejectNonObjectsWithoutMutation(t *testing.T) {
	handler, runs := newRuntimeHandlerTest(t)
	created := performJSON(
		t, handler, http.MethodPost, "/v1/sessions", `{}`,
	)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var currentSession session.Session
	if err := json.Unmarshal(created.Body.Bytes(), &currentSession); err != nil {
		t.Fatal(err)
	}
	beforeSessions := listRuntimeSessions(t, handler)
	beforeSession := getRuntimeSession(t, handler, currentSession.ID)
	beforeRuns, err := runs.List(context.Background(), runtime.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{
		"/v1/sessions",
		"/v1/sessions/gc",
		"/v1/sessions/" + currentSession.ID + "/turns",
		"/v1/sessions/" + currentSession.ID + ":reconcile",
		"/v1/agent/run",
		"/v1/runs",
		"/v1/runs/gc",
	}
	for _, path := range paths {
		for _, body := range []string{`null`, `[]`, `"value"`} {
			response := performJSON(
				t, handler, http.MethodPost, path, body,
			)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(
					response.Body.String(), `"code":"invalid_request"`,
				) {
				t.Fatalf(
					"path=%s body=%s status=%d response=%s",
					path, body, response.Code, response.Body.String(),
				)
			}
		}
	}

	afterSessions := listRuntimeSessions(t, handler)
	afterSession := getRuntimeSession(t, handler, currentSession.ID)
	afterRuns, err := runs.List(context.Background(), runtime.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeSessions, afterSessions) {
		t.Fatalf(
			"invalid object bodies mutated Sessions:\nbefore=%#v\nafter=%#v",
			beforeSessions, afterSessions,
		)
	}
	if !reflect.DeepEqual(beforeSession, afterSession) {
		t.Fatalf(
			"invalid object bodies mutated Session:\nbefore=%#v\nafter=%#v",
			beforeSession, afterSession,
		)
	}
	if !reflect.DeepEqual(beforeRuns, afterRuns) {
		t.Fatalf(
			"invalid object bodies mutated Runs:\nbefore=%#v\nafter=%#v",
			beforeRuns, afterRuns,
		)
	}
}

func TestRuntimeHandlerObjectDTOsRejectExplicitNullsWithoutMutation(
	t *testing.T,
) {
	handler, runs := newRuntimeHandlerTest(t)
	created := performJSON(
		t, handler, http.MethodPost, "/v1/sessions", `{}`,
	)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var currentSession session.Session
	if err := json.Unmarshal(created.Body.Bytes(), &currentSession); err != nil {
		t.Fatal(err)
	}
	beforeSessions := listRuntimeSessions(t, handler)
	beforeSession := getRuntimeSession(t, handler, currentSession.ID)
	beforeRuns, err := runs.List(context.Background(), runtime.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		path   string
		bodies []string
	}{
		{
			path:   "/v1/sessions",
			bodies: []string{`{"retention":null}`},
		},
		{
			path: "/v1/sessions/gc",
			bodies: []string{
				`{"older_than_hours":null}`,
				`{"limit":null}`,
				`{"apply":null}`,
			},
		},
		{
			path: "/v1/sessions/" + currentSession.ID + "/turns",
			bodies: []string{
				`{"profile_id":null}`,
				`{"model_options":null}`,
				`{"model_options":{"max_output_tokens":null}}`,
			},
		},
		{
			path: "/v1/sessions/" + currentSession.ID + ":reconcile",
			bodies: []string{
				`{"terminate":null}`,
				`{"acknowledge_unknown":null}`,
			},
		},
		{
			path: "/v1/agent/run",
			bodies: []string{
				`{"profile_id":"api","input":"x","labels":null}`,
				`{"profile_id":"api","input":"x","labels":{"key":null}}`,
				`{"profile_id":"api","input":"x","budget":null}`,
				`{"profile_id":"api","input":"x","budget":{"max_rounds":null}}`,
			},
		},
		{
			path: "/v1/runs",
			bodies: []string{
				`{"kind":null}`,
				`{"kind":"agent","profile_id":"api","input":"x","budget":null}`,
				`{"kind":"agent","profile_id":"api","input":"x","budget":{"max_tool_calls":null}}`,
				`{"kind":"session","profile_id":"api","input":"x","model_options":null}`,
				`{"kind":"session","profile_id":"api","input":"x","model_options":{"temperature":null}}`,
				`{"kind":"agent","profile_id":"api","input":"x","labels":{"key":null}}`,
			},
		},
		{
			path: "/v1/runs/gc",
			bodies: []string{
				`{"older_than":null}`,
				`{"limit":null}`,
				`{"apply":null}`,
			},
		},
	}
	for _, testCase := range testCases {
		for _, body := range testCase.bodies {
			response := performJSON(
				t, handler, http.MethodPost, testCase.path, body,
			)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(
					response.Body.String(), `"code":"invalid_request"`,
				) ||
				!strings.Contains(
					response.Body.String(), `"phase":"transport"`,
				) {
				t.Fatalf(
					"path=%s body=%s status=%d response=%s",
					testCase.path, body,
					response.Code, response.Body.String(),
				)
			}
		}
	}

	afterSessions := listRuntimeSessions(t, handler)
	afterSession := getRuntimeSession(t, handler, currentSession.ID)
	afterRuns, err := runs.List(context.Background(), runtime.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeSessions, afterSessions) {
		t.Fatalf(
			"explicit nulls mutated Sessions:\nbefore=%#v\nafter=%#v",
			beforeSessions, afterSessions,
		)
	}
	if !reflect.DeepEqual(beforeSession, afterSession) {
		t.Fatalf(
			"explicit nulls mutated Session:\nbefore=%#v\nafter=%#v",
			beforeSession, afterSession,
		)
	}
	if !reflect.DeepEqual(beforeRuns, afterRuns) {
		t.Fatalf(
			"explicit nulls mutated Runs:\nbefore=%#v\nafter=%#v",
			beforeRuns, afterRuns,
		)
	}
}

func TestRuntimeHandlerValidatesFiltersAndAgentBudgets(t *testing.T) {
	handler, services := newRuntimeHandlerTest(t)
	for _, path := range []string{
		"/v1/sessions?state=future",
		"/v1/sessions?state=",
		"/v1/sessions?unknown=value",
		"/v1/sessions?state=idle&state=active",
		"/v1/sessions?state=idle;unknown=value",
		"/v1/runs?state=future",
		"/v1/runs?state=",
		"/v1/runs?kind=future",
		"/v1/runs?kind=",
		"/v1/runs?limit=invalid",
		"/v1/runs?limit=",
		"/v1/runs?limit=0",
		"/v1/runs?limit=1001",
		"/v1/runs?unknown=value",
		"/v1/runs?state=queued&state=running",
		"/v1/runs?kind=agent&kind=session",
		"/v1/runs?limit=1&limit=2",
		"/v1/runs?state=queued;kind=agent",
	} {
		response := performJSON(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf(
				"path=%s status=%d body=%s",
				path, response.Code, response.Body.String(),
			)
		}
	}
	for _, body := range []string{
		`{"profile_id":"api","input":"x","budget":{"max_rounds":129}}`,
		`{"profile_id":"api","input":"x","budget":{"max_tool_calls":1025}}`,
		`{"profile_id":"api","input":"x","budget":{"max_total_tokens":-1}}`,
		`{"profile_id":"api","input":"x","budget":{"max_wall_time":999999999}}`,
		`{"profile_id":"api","input":"x","budget":{"max_wall_time":86400000000001}}`,
		`{"kind":"agent","profile_id":"api","input":"x","budget":{"max_rounds":129}}`,
		`{"kind":"agent","profile_id":"api","input":"x","cwd":"/tmp"}`,
	} {
		response := performJSON(
			t, handler, http.MethodPost, "/v1/agent/run", body,
		)
		if strings.Contains(body, `"kind"`) {
			response = performJSON(
				t, handler, http.MethodPost, "/v1/runs", body,
			)
		}
		if response.Code != http.StatusBadRequest {
			t.Fatalf(
				"body=%s status=%d response=%s",
				body, response.Code, response.Body.String(),
			)
		}
	}
	partial := performJSON(
		t, handler, http.MethodPost, "/v1/runs",
		`{"kind":"agent","profile_id":"api","input":"x","budget":{"max_rounds":32}}`,
	)
	if partial.Code != http.StatusAccepted {
		t.Fatalf(
			"partial status=%d body=%s", partial.Code, partial.Body.String(),
		)
	}
	var record runtime.Record
	if err := json.Unmarshal(partial.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Request.AgentBudget.MaxRounds != 32 ||
		record.Request.AgentBudget.MaxToolCalls != agent.DefaultBudget().MaxToolCalls ||
		record.Request.AgentBudget.MaxWallTime != agent.DefaultBudget().MaxWallTime {
		t.Fatalf("budget=%#v", record.Request.AgentBudget)
	}
	values, err := services.List(context.Background(), runtime.ListFilter{})
	if err != nil || len(values) != 1 {
		t.Fatalf("runs=%#v error=%v", values, err)
	}
}

func TestRuntimeHandlerGCRejectsExplicitZeroAndDurationOverflow(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	for _, test := range []struct {
		path string
		body string
	}{
		{"/v1/sessions/gc", `{"limit":0}`},
		{"/v1/sessions/gc", `{"older_than_hours":0}`},
		{"/v1/sessions/gc", `{"older_than_hours":9223372036854775807}`},
		{"/v1/runs/gc", `{"limit":0}`},
		{"/v1/runs/gc", `{"older_than":""}`},
	} {
		response := performJSON(
			t, handler, http.MethodPost, test.path, test.body,
		)
		if response.Code != http.StatusBadRequest {
			t.Fatalf(
				"path=%s body=%s status=%d response=%s",
				test.path, test.body, response.Code, response.Body.String(),
			)
		}
	}
	for _, path := range []string{"/v1/sessions/gc", "/v1/runs/gc"} {
		response := performJSON(t, handler, http.MethodPost, path, `{}`)
		if response.Code != http.StatusOK {
			t.Fatalf(
				"path=%s status=%d body=%s",
				path, response.Code, response.Body.String(),
			)
		}
	}
}

func TestRuntimeHandlerCancelUsesStrictJSONAndCanonicalConflict(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	created := performJSON(
		t, handler, http.MethodPost, "/v1/runs",
		`{"kind":"agent","profile_id":"api","input":"x"}`,
	)
	if created.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", created.Code, created.Body.String())
	}
	var record runtime.Record
	if err := json.Unmarshal(created.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}

	invalid := performJSON(
		t, handler, http.MethodPost,
		"/v1/runs/"+record.ID+":cancel", `{"unexpected":true}`,
	)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	resume := performJSON(
		t, handler, http.MethodPost,
		"/v1/runs/"+record.ID+":resume", `{}`,
	)
	if resume.Code != http.StatusConflict ||
		!strings.Contains(resume.Body.String(), `"code":"conflict"`) {
		t.Fatalf("status=%d body=%s", resume.Code, resume.Body.String())
	}
	cancelled := performJSON(
		t, handler, http.MethodPost,
		"/v1/runs/"+record.ID+":cancel", `{}`,
	)
	if cancelled.Code != http.StatusOK {
		t.Fatalf(
			"status=%d body=%s", cancelled.Code, cancelled.Body.String(),
		)
	}
}

func TestRuntimeHandlerRunEmptyObjectControlsRejectOtherJSONWithoutMutation(
	t *testing.T,
) {
	handler, services := newRuntimeHandlerTest(t)
	created := performJSON(
		t, handler, http.MethodPost, "/v1/runs",
		`{"kind":"agent","profile_id":"api","input":"x"}`,
	)
	if created.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", created.Code, created.Body.String())
	}
	var record runtime.Record
	if err := json.Unmarshal(created.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	before, err := services.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range []string{"cancel", "reconcile"} {
		for _, body := range []string{
			`null`,
			`[]`,
			`"value"`,
			`{"unexpected":null}`,
			`{"unexpected":true}`,
		} {
			response := performJSON(
				t, handler, http.MethodPost,
				"/v1/runs/"+record.ID+":"+action, body,
			)
			if response.Code != http.StatusBadRequest ||
				!strings.Contains(
					response.Body.String(), `"code":"invalid_request"`,
				) {
				t.Fatalf(
					"action=%s body=%s status=%d response=%s",
					action, body, response.Code, response.Body.String(),
				)
			}
		}
	}
	after, err := services.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("invalid control body mutated Run:\nbefore=%#v\nafter=%#v", before, after)
	}
}

func TestRuntimeHandlerRunResumeAcceptsAnyJSONValue(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	created := performJSON(
		t, handler, http.MethodPost, "/v1/runs",
		`{"kind":"agent","profile_id":"api","input":"x"}`,
	)
	if created.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", created.Code, created.Body.String())
	}
	var record runtime.Record
	if err := json.Unmarshal(created.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`null`,
		`[]`,
		`{"unexpected":true}`,
		`"resume"`,
	} {
		response := performJSON(
			t, handler, http.MethodPost,
			"/v1/runs/"+record.ID+":resume", body,
		)
		if response.Code != http.StatusConflict ||
			!strings.Contains(response.Body.String(), `"code":"conflict"`) {
			t.Fatalf(
				"body=%s status=%d response=%s",
				body, response.Code, response.Body.String(),
			)
		}
	}
}

func TestRuntimeHandlerSessionQuerySubresourcesRequireExactPathArity(
	t *testing.T,
) {
	handler, _ := newRuntimeHandlerTest(t)
	for _, path := range []string{
		"/v1/sessions/session_invalid/messages/extra",
		"/v1/sessions/session_invalid/events/extra",
		"/v1/sessions/session_invalid/executions/extra/more",
		"/v1/sessions/session_invalid/watch/extra",
	} {
		response := performJSON(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf(
				"path=%s status=%d body=%s",
				path, response.Code, response.Body.String(),
			)
		}
	}
}

func TestRuntimeHandlerSessionCreateSeparatesValidationFromStoreFailure(
	t *testing.T,
) {
	handler, _, root := newRuntimeHandlerTestFixture(t)
	values, err := handler.sessions.List(session.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("fixture unexpectedly has Sessions: %#v", values)
	}
	sessionsRoot := filepath.Join(root, "sessions")
	if err := os.RemoveAll(sessionsRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionsRoot, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := performJSON(
		t, handler, http.MethodPost, "/v1/sessions",
		`{"retention":"forever"}`,
	)
	if invalid.Code != http.StatusBadRequest ||
		!strings.Contains(invalid.Body.String(), `"code":"invalid_request"`) {
		t.Fatalf(
			"invalid status=%d body=%s",
			invalid.Code, invalid.Body.String(),
		)
	}
	data, err := os.ReadFile(sessionsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "blocked" {
		t.Fatalf("invalid retention mutated broken Store marker: %q", data)
	}
	failed := performJSON(
		t, handler, http.MethodPost, "/v1/sessions",
		`{"retention":"standard"}`,
	)
	if failed.Code != http.StatusInternalServerError ||
		!strings.Contains(failed.Body.String(), `"code":"internal"`) {
		t.Fatalf(
			"store failure status=%d body=%s",
			failed.Code, failed.Body.String(),
		)
	}
}

func TestRuntimeHandlerUnknownSessionSubresourceDoesNotMutate(
	t *testing.T,
) {
	handler, _ := newRuntimeHandlerTest(t)
	created := performJSON(
		t, handler, http.MethodPost, "/v1/sessions", `{}`,
	)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var value session.Session
	if err := json.Unmarshal(created.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	before, err := handler.sessions.Get(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	response := performJSON(
		t, handler, http.MethodPost,
		"/v1/sessions/"+value.ID+"/unknown",
		`{}`,
	)
	if response.Code != http.StatusNotFound {
		t.Fatalf(
			"status=%d body=%s", response.Code, response.Body.String(),
		)
	}
	after, err := handler.sessions.Get(value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("unknown route mutated Session: before=%#v after=%#v", before, after)
	}
}

func TestWatchErrorsDistinguishNotFoundFromInternal(t *testing.T) {
	sessionID := "session_00000000000000000000000000000000"
	runID := "run_00000000000000000000000000000000"
	injected := errors.New("injected watch failure")
	for _, testCase := range []struct {
		name string
		err  *contract.RuntimeError
		code contract.ErrorCode
	}{
		{
			name: "session_not_found",
			err:  sessionWatchError(sessionID, os.ErrNotExist),
			code: contract.ErrorNotFound,
		},
		{
			name: "session_internal",
			err:  sessionWatchError(sessionID, injected),
			code: contract.ErrorInternal,
		},
		{
			name: "session_joined_internal",
			err: sessionWatchError(
				sessionID, errors.Join(os.ErrNotExist, injected),
			),
			code: contract.ErrorInternal,
		},
		{
			name: "run_not_found",
			err:  runWatchError(runID, runtime.ErrNotFound),
			code: contract.ErrorNotFound,
		},
		{
			name: "run_internal",
			err:  runWatchError(runID, injected),
			code: contract.ErrorInternal,
		},
		{
			name: "run_joined_internal",
			err: runWatchError(
				runID, errors.Join(runtime.ErrNotFound, injected),
			),
			code: contract.ErrorInternal,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if testCase.err.Code != testCase.code {
				t.Fatalf("error=%#v, want code=%s", testCase.err, testCase.code)
			}
			if testCase.code == contract.ErrorInternal &&
				testCase.err.Phase != contract.PhaseTransport {
				t.Fatalf("internal watch error=%#v", testCase.err)
			}
		})
	}
}

func TestRuntimeHandlerReturnsNotFoundForMissingQueryParents(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_00000000000000000000000000000000"
	for _, path := range []string{
		"/v1/sessions/" + sessionID,
		"/v1/sessions/" + sessionID + "/messages",
		"/v1/sessions/" + sessionID + "/events",
		"/v1/sessions/" + sessionID + "/executions",
		"/v1/sessions/" + sessionID + "/watch",
		"/v1/runs/" + runID,
		"/v1/runs/" + runID + "/events",
	} {
		response := performJSON(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusNotFound {
			t.Fatalf(
				"path=%s status=%d body=%s",
				path, response.Code, response.Body.String(),
			)
		}
		if !strings.Contains(response.Body.String(), `"code":"not_found"`) {
			t.Fatalf("path=%s body=%s", path, response.Body.String())
		}
	}
}

func TestRuntimeHandlerRejectsMalformedResourceIDs(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	for _, path := range []string{
		"/v1/sessions/session_invalid",
		"/v1/sessions/session_invalid/messages",
		"/v1/runs/run_invalid",
		"/v1/runs/run_invalid/events",
	} {
		response := performJSON(t, handler, http.MethodGet, path, "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf(
				"path=%s status=%d body=%s",
				path, response.Code, response.Body.String(),
			)
		}
	}
}

func TestRuntimeHandlerReturnsNotFoundBeforeMissingResourceControl(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_00000000000000000000000000000000"
	for _, path := range []string{
		"/v1/sessions/" + sessionID + ":reconcile",
		"/v1/runs/" + runID + ":cancel",
		"/v1/runs/" + runID + ":resume",
		"/v1/runs/" + runID + ":reconcile",
	} {
		response := performJSON(t, handler, http.MethodPost, path, `{}`)
		if response.Code != http.StatusNotFound ||
			!strings.Contains(response.Body.String(), `"code":"not_found"`) {
			t.Fatalf(
				"path=%s status=%d body=%s",
				path, response.Code, response.Body.String(),
			)
		}
	}
}

func TestRuntimeHandlerDurableSessionCarriesModelOptionsAndRedactsPrivateSnapshot(
	t *testing.T,
) {
	handler, services := newRuntimeHandlerTest(t)
	response := performJSON(
		t, handler, http.MethodPost, "/v1/runs",
		`{
			"kind":"session",
			"profile_id":"api",
			"input":"hello",
			"model_options":{"max_output_tokens":42,"temperature":0.25}
		}`,
	)
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var record runtime.Record
	if err := json.Unmarshal(response.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.Request.ModelOptions.MaxOutputTokens == nil ||
		*record.Request.ModelOptions.MaxOutputTokens != 42 ||
		record.Request.ModelOptions.Temperature == nil ||
		*record.Request.ModelOptions.Temperature != 0.25 {
		t.Fatalf("request=%#v", record.Request)
	}
	stored, err := services.Get(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("private_request")) ||
		bytes.Contains(data, []byte("base_prompt")) {
		t.Fatalf("public Run response leaked private request: %s", data)
	}
	privateResponse := performJSON(
		t, handler, http.MethodPost, "/v1/runs",
		`{
			"kind":"session",
			"profile_id":"cli",
			"input":"hello",
			"cwd":"/"
		}`,
	)
	if privateResponse.Code != http.StatusAccepted {
		t.Fatalf(
			"private status=%d body=%s",
			privateResponse.Code, privateResponse.Body.String(),
		)
	}
	if strings.Contains(
		privateResponse.Body.String(), "http-private-base-prompt",
	) || strings.Contains(privateResponse.Body.String(), "private_request") {
		t.Fatalf("private snapshot leaked: %s", privateResponse.Body.String())
	}
}

func newRuntimeHandlerTest(
	t *testing.T,
) (*RuntimeHandler, *runtime.Service) {
	t.Helper()
	handler, runs, _ := newRuntimeHandlerTestFixture(t)
	return handler, runs
}

func newRuntimeHandlerTestFixture(
	t *testing.T,
) (*RuntimeHandler, *runtime.Service, string) {
	t.Helper()
	commandCatalog, err := runtimecommand.NewCatalog(
		map[string]runtimecommand.Profile{
			"cli": {
				Command: "codex",
				Prompt:  "http-private-base-prompt",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	modelCatalog, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver:   model.DriverOpenAI,
			Endpoint: "https://example.invalid/v1/chat/completions",
			Model:    "fixture",
			Headers: map[string]string{
				"Authorization": "${KEY}",
			},
			Timeout: "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commandCatalog, modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	generator := stubGenerator{result: contract.ModelResult{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: "done",
		},
		FinishReason: contract.FinishStop,
	}}
	root := t.TempDir()
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	runStore, err := sqlitestore.Open(
		filepath.Join(root, "state", "runtime.db"), sqlitestore.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: runStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: tools,
				Store: runStore, Sessions: sessions,
			},
			runtime.KindSession: &runtime.SessionExecutor{
				Profiles: profiles, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	handler, err := NewRuntimeHandler(RuntimeServices{
		Model: NewHandler(generator), Sessions: sessions, Runs: runs,
		AgentBudget: agent.DefaultBudget(), SettledRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, runs, root
}

func performJSON(
	t *testing.T,
	handler http.Handler,
	method, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func listRuntimeSessions(
	t *testing.T,
	handler http.Handler,
) []session.Session {
	t.Helper()
	response := performJSON(t, handler, http.MethodGet, "/v1/sessions", "")
	if response.Code != http.StatusOK {
		t.Fatalf(
			"list Sessions status=%d body=%s",
			response.Code, response.Body.String(),
		)
	}
	var value struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value.Sessions
}

func getRuntimeSession(
	t *testing.T,
	handler http.Handler,
	sessionID string,
) session.Session {
	t.Helper()
	response := performJSON(
		t, handler, http.MethodGet, "/v1/sessions/"+sessionID, "",
	)
	if response.Code != http.StatusOK {
		t.Fatalf(
			"get Session status=%d body=%s",
			response.Code, response.Body.String(),
		)
	}
	var value session.Session
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}
