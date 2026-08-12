package faux

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"
)

type Server struct {
	server    *httptest.Server
	scenarios map[string]HTTPScenario
	mu        sync.Mutex
	attempts  map[string]int
}

func NewServer(scenarios []HTTPScenario) (*Server, error) {
	values := make(map[string]HTTPScenario, len(scenarios))
	for _, current := range scenarios {
		if current.Name == "" {
			return nil, fmt.Errorf("HTTP scenario name is required")
		}
		if _, exists := values[current.Name]; exists {
			return nil, fmt.Errorf("duplicate HTTP scenario %q", current.Name)
		}
		values[current.Name] = current
	}
	result := &Server{scenarios: values, attempts: map[string]int{}}
	result.server = httptest.NewServer(http.HandlerFunc(result.serveHTTP))
	return result, nil
}

func (s *Server) Close() {
	if s != nil && s.server != nil {
		s.server.Close()
	}
}

func (s *Server) URL(name string) string {
	return s.server.URL + "?scenario=" + url.QueryEscape(name)
}

func (s *Server) Attempts(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts[name]
}

func (s *Server) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	name := request.URL.Query().Get("scenario")
	current, exists := s.scenarios[name]
	if !exists {
		http.Error(writer, "unknown faux scenario", http.StatusNotFound)
		return
	}
	s.mu.Lock()
	s.attempts[name]++
	s.mu.Unlock()
	for key, value := range current.Headers {
		writer.Header().Set(key, value)
	}
	writer.WriteHeader(current.Status)
	flusher, _ := writer.(http.Flusher)
	for _, chunk := range current.Chunks {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
		if current.ChunkDelayMS > 0 {
			if !waitForRequest(request.Context(), time.Duration(current.ChunkDelayMS)*time.Millisecond) {
				return
			}
		}
	}
	if current.CloseEarly {
		panic(http.ErrAbortHandler)
	}
}

func waitForRequest(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
