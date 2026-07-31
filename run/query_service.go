package run

import (
	"context"
	"fmt"

	"github.com/yy003x/runtime/contract"
)

// QueryService exposes the Store-backed Run query and GC surface without
// composing execution or maintenance executors.
type QueryService struct {
	service *Service
}

// NewQueryService constructs a Run query service without fake executors. The
// caller retains ownership of store when construction fails; Close transfers
// to the Store on success.
func NewQueryService(store Store) (*QueryService, error) {
	if store == nil {
		return nil, fmt.Errorf("run store is required")
	}
	return &QueryService{
		service: &Service{store: store},
	}, nil
}

func (service *QueryService) Get(
	ctx context.Context,
	runID string,
) (Record, error) {
	return service.service.Get(ctx, runID)
}

func (service *QueryService) List(
	ctx context.Context,
	filter ListFilter,
) ([]Record, error) {
	return service.service.List(ctx, filter)
}

func (service *QueryService) Events(
	ctx context.Context,
	runID string,
	afterSequence uint64,
	limit int,
) ([]contract.Event, error) {
	return service.service.Events(ctx, runID, afterSequence, limit)
}

func (service *QueryService) Watch(
	ctx context.Context,
	runID string,
	afterSequence uint64,
	sink contract.EventSink,
) (Record, error) {
	return service.service.Watch(ctx, runID, afterSequence, sink)
}

func (service *QueryService) GC(
	ctx context.Context,
	options GCOptions,
) (GCResult, error) {
	return service.service.GC(ctx, options)
}

func (service *QueryService) Close() error {
	return service.service.Close()
}
