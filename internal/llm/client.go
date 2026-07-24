package llm

import (
	"context"
)

type Client interface {
	Generate(ctx context.Context, req Request) (Response, error)
}

type StreamEvent struct {
	Delta string
}

type StreamClient interface {
	Client
	GenerateStream(ctx context.Context, req Request, emit func(StreamEvent) error) (Response, error)
}
