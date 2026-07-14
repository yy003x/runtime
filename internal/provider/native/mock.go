package native

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type MockClient struct {
	Latency   time.Duration
	Responses []string
	DoneAfter int
	mu        sync.Mutex
	calls     int
}

func (m *MockClient) Generate(ctx context.Context, request Request) (Response, error) {
	latency := m.Latency
	if latency <= 0 {
		latency = 5 * time.Millisecond
	}
	timer := time.NewTimer(latency)
	select {
	case <-timer.C:
	case <-ctx.Done():
		timer.Stop()
		return Response{}, ctx.Err()
	}
	last := ""
	if len(request.Messages) > 0 {
		last = strings.ToLower(request.Messages[len(request.Messages)-1].Content)
	}
	if strings.Contains(last, "timeout") {
		<-ctx.Done()
		return Response{}, ctx.Err()
	}
	if strings.Contains(last, "fail") {
		return Response{}, fmt.Errorf("%w: mock upstream rejected prompt", ErrUpstream)
	}
	m.mu.Lock()
	m.calls++
	call := m.calls
	m.mu.Unlock()
	text := fmt.Sprintf("round %d processed %d messages", request.Round+1, len(request.Messages))
	if call <= len(m.Responses) {
		text = m.Responses[call-1]
	}
	doneAfter := m.DoneAfter
	if doneAfter <= 0 {
		doneAfter = 1
	}
	return Response{Message: Message{Role: "assistant", Content: text}, Done: call >= doneAfter}, nil
}
