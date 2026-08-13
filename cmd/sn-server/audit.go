package main

import (
	"net/http"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
)

type auditResponseWriter struct {
	http.ResponseWriter
	status int
}

func (writer *auditResponseWriter) WriteHeader(status int) {
	if writer.status != 0 {
		return
	}
	writer.status = status
	writer.ResponseWriter.WriteHeader(status)
}

func (writer *auditResponseWriter) Write(value []byte) (int, error) {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	return writer.ResponseWriter.Write(value)
}

type auditFlushingResponseWriter struct {
	*auditResponseWriter
}

func (writer *auditFlushingResponseWriter) Flush() {
	if writer.status == 0 {
		writer.WriteHeader(http.StatusOK)
	}
	writer.ResponseWriter.(http.Flusher).Flush()
}

func auditHTTP(logsDir string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		audited := &auditResponseWriter{ResponseWriter: writer}
		completed := false
		var downstream http.ResponseWriter = audited
		if _, ok := writer.(http.Flusher); ok {
			downstream = &auditFlushingResponseWriter{auditResponseWriter: audited}
		}
		defer func() {
			status := audited.status
			if !completed && status < http.StatusBadRequest {
				status = http.StatusInternalServerError
			} else if status == 0 {
				status = http.StatusOK
			}
			action, targets := httpAuditIntent(request.Method, request.URL.Path)
			outcome := "succeeded"
			if status >= http.StatusBadRequest {
				outcome = "failed"
			}
			_ = executionlog.AppendAudit(logsDir, executionlog.AuditRecord{
				Time: time.Now(), Source: "sn-server", Namespace: "http",
				Action: action, Outcome: outcome, Targets: targets,
				HTTPStatus: status,
			})
		}()
		next.ServeHTTP(downstream, request)
		completed = true
	})
}

func httpAuditIntent(method string, path string) (string, map[string]string) {
	method = normalizedHTTPMethod(method)
	switch path {
	case "/healthz":
		return method + " health", nil
	case "/readyz":
		return method + " readiness", nil
	}
	segments := strings.Split(strings.Trim(path, "/"), "/")
	if len(segments) < 2 || segments[0] != "v1" {
		return method + " unknown", nil
	}
	targets := make(map[string]string)
	route := "unknown"
	switch segments[1] {
	case "model":
		if len(segments) == 3 && segments[2] == "generate" {
			route = "model.generate"
		}
	case "agent":
		if len(segments) == 3 && segments[2] == "run" {
			route = "agent.run"
		}
	case "sessions":
		route, targets = auditResourceRoute("session", segments[2:])
	case "runs":
		route, targets = auditResourceRoute("run", segments[2:])
	}
	if len(targets) == 0 {
		targets = nil
	}
	return method + " " + route, targets
}

func auditResourceRoute(
	resource string,
	segments []string,
) (string, map[string]string) {
	plural := resource + "s"
	if len(segments) == 0 {
		return plural, nil
	}
	if len(segments) == 1 && segments[0] == "gc" {
		return plural + ".gc", nil
	}
	value := segments[0]
	action := ""
	if base, suffix, found := strings.Cut(value, ":"); found {
		value, action = base, suffix
	}
	targets := make(map[string]string)
	if identity.Validate(value, resource) == nil {
		targets[resource+"_id"] = value
	}
	route := resource
	if action != "" {
		if auditResourceActionAllowed(resource, action) {
			route += "." + action
		} else {
			route += ".unknown"
		}
	} else if len(segments) > 1 && segments[1] != "" {
		if auditResourceSubresourceAllowed(resource, segments[1]) {
			route += "." + segments[1]
			if resource == "session" && segments[1] == "executions" &&
				len(segments) == 3 &&
				identity.Validate(segments[2], "execution") == nil {
				targets["execution_id"] = segments[2]
			} else if len(segments) > 2 {
				route = resource + ".unknown"
			}
		} else {
			route += ".unknown"
		}
	}
	return route, targets
}

func normalizedHTTPMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodHead, http.MethodOptions:
		return method
	default:
		return "OTHER"
	}
}

func auditResourceActionAllowed(resource string, action string) bool {
	switch resource {
	case "session":
		return action == "reconcile"
	case "run":
		return action == "cancel" || action == "resume" || action == "reconcile"
	default:
		return false
	}
}

func auditResourceSubresourceAllowed(resource string, subresource string) bool {
	switch resource {
	case "session":
		return subresource == "messages" || subresource == "events" ||
			subresource == "executions" || subresource == "watch" ||
			subresource == "turns"
	case "run":
		return subresource == "events"
	default:
		return false
	}
}
