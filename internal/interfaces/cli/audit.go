package cli

import (
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/domain/profileid"
	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
	"github.com/yy003x/runtime/pkg/contract"
)

var auditTmuxIDPattern = regexp.MustCompile(
	`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`,
)

type controlAuditIntent struct {
	namespace string
	action    string
	targets   map[string]string
}

func appendControlAudit(
	logsDir string,
	args []string,
	commandErr error,
) {
	intent, ok := controlAuditIntentFromArgs(args)
	if !ok {
		return
	}
	record := executionlog.AuditRecord{
		Time: time.Now(), Source: "sn-cli",
		Namespace: intent.namespace, Action: intent.action,
		Outcome: "succeeded", Targets: intent.targets,
	}
	if commandErr != nil {
		record.Outcome = "failed"
		record.ErrorCode, record.ErrorPhase = auditErrorIdentity(commandErr)
	}
	_ = executionlog.AppendAudit(logsDir, record)
}

func controlAuditIntentFromArgs(args []string) (controlAuditIntent, bool) {
	if len(args) == 1 && args[0] == "doctor" {
		return controlAuditIntent{namespace: "doctor", action: "check"}, true
	}
	if len(args) < 2 {
		return controlAuditIntent{}, false
	}
	namespace, action := args[0], args[1]
	if namespace == "agent" {
		targets := make(map[string]string)
		if profileid.Validate(action) == nil {
			targets["profile_id"] = action
		}
		if value := auditOptionValue(args[2:], "--session-id"); value != "" {
			if identity.Validate(value, "session") == nil {
				targets["session_id"] = value
			}
		}
		if len(targets) == 0 {
			targets = nil
		}
		return controlAuditIntent{
			namespace: "agent", action: "run", targets: targets,
		}, true
	}
	allowed := false
	switch namespace {
	case "session":
		allowed = auditActionAllowed(action,
			"exec", "req", "open", "send", "attach", "interrupt",
			"close", "close-all",
			"reconcile", "configure", "delete", "gc",
		)
	case "tmux":
		allowed = auditActionAllowed(action,
			"open", "send", "attach", "interrupt", "stop", "stop-all",
		)
	case "run":
		allowed = auditActionAllowed(action,
			"cancel", "resume", "retry", "reconcile", "gc",
		)
	case "server":
		allowed = auditActionAllowed(action,
			"start", "stop", "update", "upgrade-check", "upgrade-activate",
		)
	}
	if !allowed {
		return controlAuditIntent{}, false
	}
	targets := make(map[string]string)
	for _, option := range []string{
		"--session-id", "--tmux-id", "--run-id", "--execution-id",
	} {
		if value := auditOptionValue(args[2:], option); value != "" {
			key, valid := auditTargetIdentity(option, value)
			if valid {
				targets[key] = value
			}
		}
	}
	if auditActionAllowed(action, "exec", "req", "open") && len(args) > 2 &&
		args[2] != "" && !strings.HasPrefix(args[2], "-") &&
		profileid.Validate(args[2]) == nil {
		targets["profile_id"] = args[2]
	}
	if len(targets) == 0 {
		targets = nil
	}
	return controlAuditIntent{
		namespace: namespace, action: action, targets: targets,
	}, true
}

func auditActionAllowed(action string, allowed ...string) bool {
	for _, value := range allowed {
		if action == value {
			return true
		}
	}
	return false
}

func auditOptionValue(args []string, option string) string {
	for index := 0; index < len(args); index++ {
		if args[index] == "--" {
			break
		}
		if value, found := strings.CutPrefix(args[index], option+"="); found {
			return value
		}
		if index+1 >= len(args) {
			continue
		}
		if args[index] == option && args[index+1] != "" &&
			!strings.HasPrefix(args[index+1], "-") {
			return args[index+1]
		}
	}
	return ""
}

func auditTargetIdentity(option string, value string) (string, bool) {
	switch option {
	case "--session-id":
		return "session_id", identity.Validate(value, "session") == nil
	case "--run-id":
		return "run_id", identity.Validate(value, "run") == nil
	case "--execution-id":
		return "execution_id", identity.Validate(value, "execution") == nil
	case "--tmux-id":
		return "tmux_id", auditTmuxIDPattern.MatchString(value)
	default:
		return "", false
	}
}

func auditErrorIdentity(err error) (contract.ErrorCode, contract.ErrorPhase) {
	var runtimeErr *contract.RuntimeError
	if errors.As(err, &runtimeErr) {
		code, phase := runtimeErr.Code, runtimeErr.Phase
		if code == "" {
			code = contract.ErrorInternal
		}
		if phase == "" {
			phase = contract.PhaseRequest
		}
		return code, phase
	}
	var validationErr *cliValidationError
	if errors.As(err, &validationErr) {
		return contract.ErrorInvalidRequest, contract.PhaseRequest
	}
	return contract.ErrorInternal, contract.PhaseRequest
}
