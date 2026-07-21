package agentrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const followPollInterval = 100 * time.Millisecond

// Follow forwards only the Provider stream section of output.log and waits for
// the Run terminal state. A writer failure detaches the follower without
// cancelling the durable Run.
func (s *Service) Follow(ctx context.Context, runType, runID string, writer io.Writer) (RunSummary, error) {
	paths, err := RunPaths(s.RunsDir, runType, runID)
	if err != nil {
		return RunSummary{}, err
	}
	if writer == nil {
		writer = io.Discard
	}
	if _, err := s.Status(runType, runID); err != nil {
		return RunSummary{}, err
	}
	cursor := int64(0)
	streamStarted := false
	drain := func() error {
		chunk, readErr := readFollowChunk(paths.OutputLog, &cursor, &streamStarted)
		if readErr != nil || len(chunk) == 0 {
			return readErr
		}
		_, writeErr := writer.Write(chunk)
		return writeErr
	}
	ticker := time.NewTicker(followPollInterval)
	defer ticker.Stop()
	for {
		if err := drain(); err != nil {
			return RunSummary{RunID: runID, RunType: runType, RunDir: paths.RunDir}, fmt.Errorf("follow run %s: %w", runID, err)
		}
		status, statusErr := s.Status(runType, runID)
		if statusErr == nil && (terminalStateValue(status.State) || status.State == StateBlocked) {
			if err := drain(); err != nil {
				return summary(paths, status, false), fmt.Errorf("follow run %s: %w", runID, err)
			}
			return summary(paths, status, false), terminalStatusError(status)
		}
		if statusErr != nil {
			return RunSummary{RunID: runID, RunType: runType, RunDir: paths.RunDir}, statusErr
		}
		select {
		case <-ctx.Done():
			_, cancelErr := s.Cancel(runType, runID)
			return RunSummary{RunID: runID, RunType: runType, State: StateCancelled, RunDir: paths.RunDir}, errors.Join(ctx.Err(), cancelErr)
		case <-ticker.C:
		}
	}
}

func readFollowChunk(path string, cursor *int64, streamStarted *bool) ([]byte, error) {
	if !*streamStarted {
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		marker := bytes.Index(data, []byte(outputStreamMarker))
		if marker < 0 {
			return nil, nil
		}
		*cursor = int64(marker + len(outputStreamMarker))
		*streamStarted = true
		chunk := append([]byte(nil), data[*cursor:]...)
		*cursor = int64(len(data))
		return chunk, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < *cursor {
		return nil, fmt.Errorf("output.log shrank from %d to %d bytes", *cursor, info.Size())
	}
	if info.Size() == *cursor {
		return nil, nil
	}
	if _, err := file.Seek(*cursor, io.SeekStart); err != nil {
		return nil, err
	}
	chunk, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	*cursor += int64(len(chunk))
	return chunk, nil
}
