package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/profile"
)

const maxCLIProjectionBytes = 256 << 10

type projection struct {
	modelMessages []contract.Message
	commandPrompt string
	manifest      ContextManifest
}

func buildProjection(
	entry profile.Entry,
	sessionID, turnID, runID, executionID, taskID string,
	request RunRequest,
	records []MessageRecord,
	now time.Time,
) (projection, *contract.RuntimeError) {
	messages := make([]contract.Message, 0, len(records))
	for _, record := range records {
		messages = append(messages, canonicalMessageRecord(record))
	}
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
		window, reserved, inputBudget, configured :=
			entry.Model.EffectiveContextBudget()
		manifest.ContextWindowTokens = window
		manifest.ReservedOutputTokens = reserved
		if configured {
			manifest.CapacitySource = "profile"
			manifest.InputBudgetTokens = inputBudget
			if manifest.InputBudgetTokens <= 0 {
				return projection{}, sessionRuntimeError(
					contract.ErrorInvalidRequest,
					"model context policy leaves no input budget",
				)
			}
			pressureThreshold := manifest.InputBudgetTokens * 8 / 10
			if estimatedTokens > pressureThreshold {
				manifest.PressureState = "high"
			}
			if estimatedTokens > manifest.InputBudgetTokens {
				manifest.PressureState = "overflow"
				return projection{}, sessionRuntimeError(
					contract.ErrorContextOverflow,
					fmt.Sprintf(
						"estimated context %d tokens exceeds input budget %d",
						estimatedTokens, manifest.InputBudgetTokens,
					),
				)
			}
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
		message := canonicalMessageRecord(record)
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
