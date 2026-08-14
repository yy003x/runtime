package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/profile"
)

const maxCLIProjectionBytes = 256 << 10

type projection struct {
	modelMessages []contract.Message
	commandPrompt string
	manifest      ContextManifest
	summary       *SummaryRecord
}

func buildProjection(
	entry profile.Entry,
	sessionID, turnID, runID, executionID, taskID string,
	request RunRequest,
	records []MessageRecord,
	now time.Time,
) (projection, *contract.RuntimeError) {
	messages := recordsToMessages(records)
	messageJSON, err := json.Marshal(messages)
	if err != nil {
		return projection{}, sessionRuntimeError(
			contract.ErrorInternal, "encode canonical messages",
		)
	}
	estimatedTokens := estimateTokens(messageJSON)
	manifest := ContextManifest{
		SchemaVersion:         SchemaVersion,
		SessionID:             sessionID,
		TurnID:                turnID,
		RunID:                 runID,
		ExecutionID:           executionID,
		TaskID:                taskID,
		ProfileID:             entry.ID,
		ProfileKind:           entry.Kind,
		ConfigDigest:          requestConfigDigest(request, entry),
		RequestDigest:         requestDigest(request),
		BasePromptDigest:      requestBasePromptDigest(request),
		CWD:                   request.CWD,
		MessageDigest:         digest(messageJSON),
		EstimatedTokens:       estimatedTokens,
		EstimatorCompleteness: "partial",
		CapacitySource:        "unbounded",
		PressureState:         "normal",
		CurrentInputDigest:    digest([]byte(request.Input)),
		CreatedAt:             now,
	}
	if len(records) > 0 {
		manifest.MessageSequenceStart = records[0].Sequence
		manifest.MessageSequenceEnd = records[len(records)-1].Sequence
	}
	if entry.Kind == profile.KindModel {
		window, reserved, inputBudget, explicit :=
			entry.Model.EffectiveContextBudgetForRequest(
				request.ModelOptions.MaxOutputTokens,
			)
		manifest.ContextWindowTokens = window
		manifest.ReservedOutputTokens = reserved
		manifest.InputBudgetTokens = inputBudget
		manifest.CapacitySource = "conservative_default"
		if explicit {
			manifest.CapacitySource = "profile"
		}
		if manifest.InputBudgetTokens < 2 {
			return projection{}, sessionRuntimeError(
				contract.ErrorInvalidRequest,
				"model context policy leaves fewer than 2 input tokens",
			)
		}
		pressureThreshold := manifest.InputBudgetTokens * 8 / 10
		if estimatedTokens > pressureThreshold {
			manifest.PressureState = "high"
		}
		if estimatedTokens > manifest.InputBudgetTokens {
			if entry.Model != nil && entry.Model.Context.SummaryEnabled != nil &&
				*entry.Model.Context.SummaryEnabled {
				kept, dropped, ok := compressProjection(
					records, entry.Model.Context.KeepRecentTurns,
					manifest.InputBudgetTokens,
				)
				if ok {
					built, err := buildCompactionSummary(
						dropped, kept[0].Sequence, sessionID, turnID, runID,
						entry.Model.Context.KeepRecentTurns, now,
					)
					if err != nil {
						return projection{}, sessionRuntimeError(
							contract.ErrorInternal, err.Error(),
						)
					}
					compactedMessages := recordsToMessages(kept)
					compactedJSON, err := json.Marshal(compactedMessages)
					if err != nil {
						return projection{}, sessionRuntimeError(
							contract.ErrorInternal, "encode compacted messages",
						)
					}
					manifest.MessageSequenceStart = kept[0].Sequence
					manifest.MessageSequenceEnd = kept[len(kept)-1].Sequence
					manifest.MessageDigest = digest(compactedJSON)
					manifest.EstimatedTokens = estimateTokens(compactedJSON)
					manifest.PressureState = pressureState(
						manifest.EstimatedTokens, manifest.InputBudgetTokens,
					)
					manifest.CheckpointRef = built.SummaryID
					if canonical, err := json.Marshal(built); err == nil {
						manifest.CheckpointDigest = digest(canonical)
					}
					return projection{
						modelMessages: compactedMessages,
						manifest:      manifest,
						summary:       &built,
					}, nil
				}
			}
			manifest.PressureState = "overflow"
			return projection{}, sessionRuntimeError(
				contract.ErrorContextOverflow,
				fmt.Sprintf(
					"estimated context %d tokens exceeds input budget %d",
					estimatedTokens, manifest.InputBudgetTokens,
				),
			)
		}
		return projection{modelMessages: messages, manifest: manifest}, nil
	}
	prompt := projectCLIHistory(sessionID, records, request.Input)
	if len(prompt) > maxCLIProjectionBytes {
		return projection{}, sessionRuntimeError(
			contract.ErrorContextOverflow,
			fmt.Sprintf("CLI projection exceeds %d bytes", maxCLIProjectionBytes),
		)
	}
	manifest.EstimatedTokens = estimateTokens([]byte(prompt))
	manifest.CapacitySource = "runtime_cli_limit"
	manifest.InputBudgetTokens = estimateTokens(make([]byte, maxCLIProjectionBytes))
	if len(prompt) > maxCLIProjectionBytes*8/10 {
		manifest.PressureState = "high"
	}
	return projection{commandPrompt: prompt, manifest: manifest}, nil
}

func projectCLIHistory(
	sessionID string,
	records []MessageRecord,
	currentInput string,
) string {
	history := records
	if len(history) > 0 && history[len(history)-1].Message.Role == contract.RoleUser {
		history = history[:len(history)-1]
	}
	var builder strings.Builder
	fmt.Fprintf(
		&builder,
		"<runtime_session_history session_id=\"%s\">\n",
		html.EscapeString(sessionID),
	)
	for _, record := range history {
		message := cloneContractMessage(record.Message)
		messageJSON, _ := json.Marshal(message)
		fmt.Fprintf(
			&builder,
			"<turn sequence=\"%d\" role=\"%s\">%s</turn>\n",
			record.Sequence,
			html.EscapeString(string(message.Role)),
			html.EscapeString(string(messageJSON)),
		)
	}
	builder.WriteString("</runtime_session_history>\n\n<current_user_input>\n")
	builder.WriteString(html.EscapeString(currentInput))
	builder.WriteString("\n</current_user_input>")
	return builder.String()
}

func recordsToMessages(records []MessageRecord) []contract.Message {
	messages := make([]contract.Message, 0, len(records))
	for _, record := range records {
		messages = append(messages, cloneContractMessage(record.Message))
	}
	return messages
}

func pressureState(estimated, inputBudget int64) string {
	if estimated > inputBudget {
		return "overflow"
	}
	if estimated > inputBudget*8/10 {
		return "high"
	}
	return "normal"
}

// turnBoundaries returns the indexes into records at which each TurnID begins.
// The first record is always a boundary. Compaction splits only on turn
// boundaries so the kept tail is a sequence of complete turns.
func turnBoundaries(records []MessageRecord) []int {
	boundaries := make([]int, 0, 8)
	previous := ""
	for index, record := range records {
		if record.TurnID != previous {
			boundaries = append(boundaries, index)
			previous = record.TurnID
		}
	}
	return boundaries
}

// compressProjection applies deterministic context compaction by dropping a
// leading range of whole turns until the remaining tail fits the input budget.
// keepRecentTurns is the minimum number of recent turns that must be retained
// (floored to 1). It returns the kept records, the dropped records, and ok=true
// when a fitting tail exists; ok=false when even the minimum tail overflows
// (the caller must fail-closed with ErrorContextOverflow).
func compressProjection(
	records []MessageRecord,
	keepRecentTurns int,
	inputBudget int64,
) (kept, dropped []MessageRecord, ok bool) {
	if len(records) == 0 {
		return nil, nil, false
	}
	boundaries := turnBoundaries(records)
	totalTurns := len(boundaries)
	keepTurns := keepRecentTurns
	if keepTurns < 1 {
		keepTurns = 1
	}
	// Drop increasing numbers of leading turns (keep fewer recent turns) until
	// the tail fits. The first fit wins so the maximal recent context survives.
	for keptTurns := totalTurns - 1; keptTurns >= keepTurns; keptTurns-- {
		start := boundaries[totalTurns-keptTurns]
		tail := records[start:]
		encoded, err := json.Marshal(recordsToMessages(tail))
		if err != nil {
			return nil, nil, false
		}
		if estimateTokens(encoded) <= inputBudget {
			return tail, records[:start], true
		}
	}
	return nil, nil, false
}

// buildCompactionSummary builds the SummaryRecord that grounds a truncation:
// RangeStartSeq..RangeEndSeq names the dropped canonical range (exclusive end,
// equal to the first kept message sequence), and CompactedRangeDigest is the
// sha256 of the dropped messages' canonical JSON so recovery can re-verify the
// dropped prefix bit-for-bit.
func buildCompactionSummary(
	dropped []MessageRecord,
	firstKeptSeq uint64,
	sessionID, turnID, runID string,
	keepRecentTurns int,
	now time.Time,
) (SummaryRecord, error) {
	if len(dropped) == 0 {
		return SummaryRecord{}, fmt.Errorf("compaction dropped no messages")
	}
	summaryID, err := identity.New("summary")
	if err != nil {
		return SummaryRecord{}, err
	}
	droppedJSON, err := json.Marshal(recordsToMessages(dropped))
	if err != nil {
		return SummaryRecord{}, err
	}
	return SummaryRecord{
		SummaryVersion:        SummaryRecordVersion,
		SummaryID:             summaryID,
		SessionID:             sessionID,
		TurnID:                turnID,
		RunID:                 runID,
		RangeStartSeq:         dropped[0].Sequence,
		RangeEndSeq:           firstKeptSeq,
		CompactedRangeDigest:  digest(droppedJSON),
		PolicyKeepRecentTurns: keepRecentTurns,
		CreatedAt:             now,
	}, nil
}

// verifyCompaction recomputes the dropped-prefix digest from canonical records
// and returns the offset at which the kept (compacted) tail begins. The offset
// is the number of leading canonical messages the summary replaced; the kept
// tail is canonical[offset:]. It fail-closes when the range is out of bounds or
// the recomputed digest does not match, so a tampered messages.jsonl prefix is
// detected at settle/recover time.
func verifyCompaction(records []MessageRecord, summary SummaryRecord) (int, error) {
	if len(records) == 0 {
		return 0, fmt.Errorf("no canonical messages to verify compaction")
	}
	base := records[0].Sequence
	last := records[len(records)-1].Sequence
	if summary.RangeEndSeq < base || summary.RangeEndSeq > last {
		return 0, fmt.Errorf(
			"compaction range end %d outside canonical %d..%d",
			summary.RangeEndSeq, base, last,
		)
	}
	offset := int(summary.RangeEndSeq) - int(base)
	if offset >= len(records) {
		return 0, fmt.Errorf("compaction dropped every canonical message")
	}
	droppedJSON, err := json.Marshal(recordsToMessages(records[:offset]))
	if err != nil {
		return 0, err
	}
	if digest(droppedJSON) != summary.CompactedRangeDigest {
		return 0, fmt.Errorf("compacted range digest does not match canonical prefix")
	}
	return offset, nil
}

func estimateTokens(value []byte) int64 {
	if len(value) == 0 {
		return 0
	}
	return int64((len(value) + 3) / 4)
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
