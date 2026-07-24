package runtimeclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yy003x/runtime/runtimeapi"
)

func TestGenerateUsesSharedHTTPContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/llm/generate" || request.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("unexpected request: %s %s", request.Method, request.URL.Path)
		}
		var input runtimeapi.Request
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		_ = json.NewEncoder(writer).Encode(runtimeapi.Response{
			Message: runtimeapi.Message{Role: "assistant", Content: input.Prompt},
			Done:    true, Rounds: 1,
		})
	}))
	defer server.Close()

	client, err := New(Options{BaseURL: server.URL, BearerToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Generate(context.Background(), runtimeapi.Request{Profile: "p", Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "hello" {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestGenerateStreamUsesSharedEventContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/llm/generate" ||
			request.Header.Get("Authorization") != "Bearer token" ||
			request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("unexpected request: %s %s headers=%v", request.Method, request.URL.Path, request.Header)
		}
		writer.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		started := runtimeapi.Event{Sequence: 1, Type: runtimeapi.EventRequestStarted}
		completed := runtimeapi.Event{
			Sequence: 2,
			Type:     runtimeapi.EventResponseCompleted,
			Response: &runtimeapi.Response{
				Message: runtimeapi.Message{Role: "assistant", Content: "done"},
				Done:    true,
				Rounds:  1,
			},
		}
		for _, event := range []runtimeapi.Event{started, completed} {
			data, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = writer.Write([]byte("event: " + event.Type + "\ndata: " + string(data) + "\n\n"))
		}
	}))
	defer server.Close()

	client, err := New(Options{BaseURL: server.URL, BearerToken: "token"})
	if err != nil {
		t.Fatal(err)
	}
	var types []string
	response, err := client.GenerateStream(
		context.Background(),
		runtimeapi.Request{Profile: "p", Prompt: "hello"},
		func(event runtimeapi.Event) error {
			types = append(types, event.Type)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if response.Message.Content != "done" || len(types) != 2 ||
		types[0] != runtimeapi.EventRequestStarted ||
		types[1] != runtimeapi.EventResponseCompleted {
		t.Fatalf("response=%#v event types=%v", response, types)
	}
}
