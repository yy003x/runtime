package http

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
)

func TestResumeSequenceStrictInputAndExplicitZero(t *testing.T) {
	for _, test := range []struct {
		name, query string
		headers     []string
		want        uint64
		valid       bool
	}{
		{name: "default", valid: true},
		{name: "header", headers: []string{"12"}, want: 12, valid: true},
		{name: "empty header", headers: []string{""}, valid: true},
		{name: "query", query: "after_seq=7", want: 7, valid: true},
		{name: "query precedence", query: "after_seq=7", headers: []string{"12"}, want: 7, valid: true},
		{name: "zero precedence", query: "after_seq=0", headers: []string{"12"}, valid: true},
		{name: "query ignores header", query: "after_seq=0", headers: []string{"invalid"}, valid: true},
		{name: "uint64 max", query: "after_seq=18446744073709551615", want: ^uint64(0), valid: true},
		{name: "empty query", query: "after_seq=", headers: []string{"12"}},
		{name: "duplicate query", query: "after_seq=1&after_seq=2"},
		{name: "unknown query", query: "unknown=1"},
		{name: "malformed query", query: "after_seq=%zz"},
		{name: "semicolon query", query: "after_seq=1;other=2"},
		{name: "negative query", query: "after_seq=-1"},
		{name: "signed query", query: "after_seq=%2B1"},
		{name: "overflow query", query: "after_seq=18446744073709551616"},
		{name: "invalid header", headers: []string{"invalid"}},
		{name: "negative header", headers: []string{"-1"}},
		{name: "overflow header", headers: []string{"18446744073709551616"}},
		{name: "duplicate header", headers: []string{"1", "2"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/watch?"+test.query, nil)
			for _, value := range test.headers {
				request.Header.Add("Last-Event-ID", value)
			}
			writer := httptest.NewRecorder()
			got, valid := resumeSequence(writer, request)
			if valid != test.valid || (valid && got != test.want) {
				t.Fatalf("sequence=%d valid=%v want=%d/%v", got, valid, test.want, test.valid)
			}
			if !valid && (writer.Code != http.StatusBadRequest || !strings.Contains(writer.Body.String(), "invalid_request")) {
				t.Fatalf("status=%d body=%s", writer.Code, writer.Body.String())
			}
		})
	}
}

func TestEventRoutesRejectInvalidSequenceQueries(t *testing.T) {
	handler, runs := newRuntimeHandlerTest(t)
	sessionValue, err := handler.sessions.Create(session.RetentionStandard)
	if err != nil {
		t.Fatal(err)
	}
	runValue, runtimeErr := runs.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "queued",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	for _, path := range []string{
		"/v1/sessions/" + sessionValue.ID + "/messages",
		"/v1/sessions/" + sessionValue.ID + "/events",
		"/v1/sessions/" + sessionValue.ID + "/watch",
		"/v1/runs/" + runValue.ID + "/events",
	} {
		for _, query := range []string{"after_seq=", "after_seq=1&after_seq=2", "unknown=1", "after_seq=-1"} {
			for _, accept := range []string{"application/json", "text/event-stream"} {
				request := httptest.NewRequest(http.MethodGet, path+"?"+query, nil)
				request.Header.Set("Accept", accept)
				writer := httptest.NewRecorder()
				handler.ServeHTTP(writer, request)
				if writer.Code != http.StatusBadRequest || strings.Contains(writer.Header().Get("Content-Type"), "event-stream") {
					t.Fatalf("path=%s?%s accept=%s status=%d body=%s", path, query, accept, writer.Code, writer.Body.String())
				}
			}
		}
	}
}

func TestWatchHeartbeatsFlushWithoutChangingDurableEvents(t *testing.T) {
	for _, kind := range []string{"session", "run"} {
		t.Run(kind, func(t *testing.T) {
			handler, runs := newRuntimeHandlerTest(t)
			if handler.watchHeartbeatInterval != 5*time.Second {
				t.Fatalf("default heartbeat=%s", handler.watchHeartbeatInterval)
			}
			handler.watchHeartbeatInterval = 10 * time.Millisecond
			var path string
			var readEvents func() []byte
			if kind == "session" {
				value, err := handler.sessions.Create(session.RetentionStandard)
				if err != nil {
					t.Fatal(err)
				}
				path = "/v1/sessions/" + value.ID + "/watch"
				readEvents = func() []byte {
					values, err := handler.sessions.Events(value.ID, 0)
					if err != nil {
						t.Fatal(err)
					}
					data, err := json.Marshal(values)
					if err != nil {
						t.Fatal(err)
					}
					return data
				}
			} else {
				value, runtimeErr := runs.Submit(context.Background(), runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api", Input: "queued",
				})
				if runtimeErr != nil {
					t.Fatal(runtimeErr)
				}
				path = "/v1/runs/" + value.ID + "/events"
				readEvents = func() []byte {
					values, err := runs.Events(context.Background(), value.ID, 0, runtime.MaxListLimit)
					if err != nil {
						t.Fatal(err)
					}
					data, err := json.Marshal(values)
					if err != nil {
						t.Fatal(err)
					}
					return data
				}
			}
			before := readEvents()
			done := make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				defer close(done)
				handler.ServeHTTP(writer, request)
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+path+"?after_seq=18446744073709551615", nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Header.Set("Accept", "text/event-stream")
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK || response.Header.Get("X-Accel-Buffering") != "no" || response.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("status=%d headers=%v", response.StatusCode, response.Header)
			}
			reader := bufio.NewReader(response.Body)
			for i := 0; i < 2; i++ {
				line, err := reader.ReadString('\n')
				if err != nil || line != ": keep-alive\n" {
					t.Fatalf("heartbeat=%q err=%v", line, err)
				}
				line, err = reader.ReadString('\n')
				if err != nil || line != "\n" {
					t.Fatalf("boundary=%q err=%v", line, err)
				}
			}
			cancel()
			_ = response.Body.Close()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("watch did not exit after disconnect")
			}
			if after := readEvents(); string(after) != string(before) {
				t.Fatalf("heartbeats changed durable events: before=%s after=%s", before, after)
			}
		})
	}
}

func TestRunWatchReplaysExactlyAfterCursorAndSettles(t *testing.T) {
	handler, runs := newRuntimeHandlerTest(t)
	response := performJSON(t, handler, http.MethodPost, "/v1/agent/run", `{"profile_id":"api","input":"finish"}`)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	var value runtime.Record
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	events, err := runs.Events(context.Background(), value.ID, 0, runtime.MaxListLimit)
	if err != nil || len(events) < 2 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	for _, test := range []struct {
		name, query, header string
		after               uint64
	}{
		{"header", "", strconv.FormatUint(value.SettledSequence-1, 10), value.SettledSequence - 1},
		{"explicit zero", "?after_seq=0", strconv.FormatUint(value.SettledSequence, 10), 0},
		{"already settled", "", strconv.FormatUint(value.SettledSequence, 10), value.SettledSequence},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/runs/"+value.ID+"/events"+test.query, nil)
			request.Header.Set("Accept", "text/event-stream")
			request.Header.Set("Last-Event-ID", test.header)
			writer := httptest.NewRecorder()
			handler.ServeHTTP(writer, request)
			if writer.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", writer.Code, writer.Body.String())
			}
			var got, want []uint64
			for _, line := range strings.Split(writer.Body.String(), "\n") {
				if strings.HasPrefix(line, "id: ") {
					id, err := strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
					if err != nil {
						t.Fatal(err)
					}
					got = append(got, id)
				}
			}
			for _, event := range events {
				if event.Sequence > test.after {
					want = append(want, event.Sequence)
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("ids=%v want=%v body=%s", got, want, writer.Body.String())
			}
		})
	}
}

func TestSessionWatchResumesFullEventRecord(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	value, err := handler.sessions.Create(session.RetentionStandard)
	if err != nil {
		t.Fatal(err)
	}
	turn := performJSON(t, handler, http.MethodPost, "/v1/sessions/"+value.ID+"/turns",
		`{"profile_id":"api","input":"hello"}`)
	if turn.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", turn.Code, turn.Body.String())
	}
	events, err := handler.sessions.Events(value.ID, 0)
	if err != nil || len(events) < 2 {
		t.Fatalf("events=%v err=%v", events, err)
	}
	want := events[len(events)-1]
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer close(done)
		handler.ServeHTTP(writer, request)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/v1/sessions/"+value.ID+"/watch", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Last-Event-ID", strconv.FormatUint(events[len(events)-2].Sequence, 10))
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	scanner := bufio.NewScanner(response.Body)
	var got session.EventRecord
	var id uint64
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "id: ") {
			id, err = strconv.ParseUint(strings.TrimPrefix(line, "id: "), 10, 64)
			if err != nil {
				t.Fatal(err)
			}
		}
		if strings.HasPrefix(line, "data: ") {
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if id != want.Sequence || !reflect.DeepEqual(got, want) {
		t.Fatalf("id=%d event=%#v want=%#v", id, got, want)
	}
	cancel()
	_ = response.Body.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Session watch did not exit")
	}
}

type failingWatchWriter struct {
	*httptest.ResponseRecorder
	writes int
}

func (writer *failingWatchWriter) Write(data []byte) (int, error) {
	writer.writes++
	if writer.writes > 1 {
		return 0, errors.New("disconnected writer")
	}
	return writer.ResponseRecorder.Write(data)
}

func TestRunWatchWriteFailureCancelsUnderlyingWatch(t *testing.T) {
	handler, runs := newRuntimeHandlerTest(t)
	handler.watchHeartbeatInterval = 5 * time.Millisecond
	value, runtimeErr := runs.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "queued",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	writer := &failingWatchWriter{ResponseRecorder: httptest.NewRecorder()}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/runs/%s/events", value.ID), nil).WithContext(ctx)
	request.Header.Set("Accept", "text/event-stream")
	handler.ServeHTTP(writer, request)
	if ctx.Err() != nil || writer.writes != 2 {
		t.Fatalf("watch did not exit on write failure: writes=%d context=%v", writer.writes, ctx.Err())
	}
}

func TestSSEDataRejectsInvalidPayload(t *testing.T) {
	writer := httptest.NewRecorder()
	err := writeSSEData(writer, writer, 1, "session.event", session.EventRecord{
		Sequence: 1, Type: "session.event", Detail: json.RawMessage(`{`),
	})
	if err == nil || writer.Body.Len() != 0 {
		t.Fatalf("error=%v body=%s", err, writer.Body.String())
	}
}
