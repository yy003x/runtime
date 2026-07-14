package daemon

import (
	"context"
	"time"
)

func (s *Server) acquire(ctx context.Context, processID string, dependencies []Dependency, execution ExecutionEnvironment) Response {
	if processID == "" {
		return Response{Error: "acquire requires process_id"}
	}
	dependencyNames, err := s.ensureDependencies(ctx, processID, dependencies, execution)
	if err != nil {
		return Response{Error: err.Error()}
	}
	environment, err := s.executionValues(execution)
	if err != nil {
		s.releaseDependencies(processID)
		return Response{Error: err.Error()}
	}
	s.mu.Lock()
	s.processes[processID] = managedProcess{ProcessStatus: ProcessStatus{
		ID: processID, Kind: "lease", Alive: true, AuditProxy: execution.AuditProxy,
		Shim: execution.Shim, Dylib: execution.Dylib != "", StartedAt: time.Now().UTC(),
	}, Depends: dependencyNames}
	s.lastActive = time.Now()
	_ = s.persistRegistryLocked()
	s.mu.Unlock()
	return Response{OK: true, Environment: environment}
}
