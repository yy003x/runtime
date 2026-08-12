package run

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// completionCheckTimeout caps a single completion check subprocess so a hung
// verifier cannot block a run indefinitely. The run context deadline remains
// the outer cap.
const completionCheckTimeout = 10 * time.Minute

// maxCompletionCheckOutput bounds how much subprocess output is retained as
// validation evidence; the full output is still consumed but only the head is
// kept in the result detail.
const maxCompletionCheckOutput = 4096

// ValidateCompletion implements CompletionValidator. It runs each "command"
// check declared in record.Request.CompletionCriteria as a subprocess in the
// Run's CWD; any non-zero exit fails validation, so the model's claim of
// completion is never trusted alone when criteria are declared. An empty
// criteria set passes trivially (no criteria == nothing to refute).
func (executor *AgentExecutor) ValidateCompletion(
	ctx context.Context,
	record Record,
	outcome ExecutionOutcome,
) (ValidationResult, error) {
	checks := record.Request.CompletionCriteria.Checks
	if len(checks) == 0 {
		return ValidationResult{Passed: true}, nil
	}
	result := ValidationResult{Passed: true}
	for _, check := range checks {
		checkResult, err := runCompletionCheck(ctx, check, record)
		if err != nil {
			return ValidationResult{}, err
		}
		result.Checks = append(result.Checks, checkResult)
		if !checkResult.Passed {
			result.Passed = false
		}
	}
	if !result.Passed {
		failed := make([]string, 0, len(result.Checks))
		for _, check := range result.Checks {
			if !check.Passed {
				failed = append(failed, check.Name)
			}
		}
		result.Summary = fmt.Sprintf(
			"completion checks failed: %s", strings.Join(failed, ", "),
		)
	}
	return result, nil
}

func runCompletionCheck(
	ctx context.Context,
	check CompletionCheck,
	record Record,
) (CheckResult, error) {
	if check.Name == "" {
		return CheckResult{}, fmt.Errorf("completion check is missing a name")
	}
	switch check.Type {
	case "command":
		if len(check.Command) == 0 {
			return CheckResult{}, fmt.Errorf(
				"completion check %q has no command", check.Name,
			)
		}
		return runCommandCheck(ctx, check, record), nil
	default:
		return CheckResult{}, fmt.Errorf(
			"completion check %q has unsupported type %q", check.Name, check.Type,
		)
	}
}

func runCommandCheck(
	ctx context.Context,
	check CompletionCheck,
	record Record,
) CheckResult {
	checkCtx, cancel := context.WithTimeout(ctx, completionCheckTimeout)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, check.Command[0], check.Command[1:]...)
	if dir := record.Request.CWD; dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	return CheckResult{
		Name:   check.Name,
		Passed: err == nil,
		Detail: buildCheckDetail(check, output, err),
	}
}

func buildCheckDetail(check CompletionCheck, output []byte, err error) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "command: %s", strings.Join(check.Command, " "))
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			fmt.Fprintf(&builder, "\nexit: %d", exitErr.ExitCode())
		} else {
			fmt.Fprintf(&builder, "\nerror: %s", err.Error())
		}
	}
	text := strings.TrimSpace(string(output))
	if text != "" {
		if len(text) > maxCompletionCheckOutput {
			text = text[:maxCompletionCheckOutput] +
				fmt.Sprintf(" ...(%d more bytes)", len(text)-maxCompletionCheckOutput)
		}
		fmt.Fprintf(&builder, "\noutput:\n%s", text)
	}
	return builder.String()
}
