package memory

import "context"

type Retriever interface {
	Retrieve(ctx context.Context, sessionID, query string) ([]RetrievedMemory, error)
}

type StubRetriever struct{}

func (StubRetriever) Retrieve(_ context.Context, _ string, _ string) ([]RetrievedMemory, error) {
	return nil, nil
}
