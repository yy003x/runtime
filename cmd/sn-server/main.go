package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/domain/profileid"
	"github.com/yy003x/runtime/internal/infrastructure/activationgate"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
	transporthttp "github.com/yy003x/runtime/pkg/transport/http"
)

type serverConfig struct {
	Address     string
	BearerToken string
}

var fixedNamespaces = profileid.ReservedNamespaces()

func main() {
	ctx, stop := signal.NotifyContext(
		context.Background(), syscall.SIGINT, syscall.SIGTERM,
	)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Getenv); err != nil {
		log.Printf("sn-server: %v", err)
		os.Exit(1)
	}
}

func run(
	ctx context.Context,
	args []string,
	getenv func(string) string,
) (resultErr error) {
	if err := validateServerArgs(args); err != nil {
		return fmt.Errorf("arguments: %w", err)
	}
	paths, err := layout.Resolve()
	if err != nil {
		return fmt.Errorf("resolve runtime home: %w", err)
	}
	if err := requireActivationReady(paths); err != nil {
		return fmt.Errorf("activation gate: %w", err)
	}
	config, err := loadServerConfig(getenv)
	if err != nil {
		return fmt.Errorf("server config: %w", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve working directory: %w", err)
	}
	services, err := runtimebootstrap.LoadServicesWithRunRecovery(
		paths,
		cwd,
		fixedNamespaces...,
	)
	if err != nil {
		return fmt.Errorf("initialize SN Runtime: %w", err)
	}
	defer func() {
		if err := services.Runs.Close(); err != nil {
			resultErr = errors.Join(
				resultErr, fmt.Errorf("close Run service: %w", err),
			)
		}
	}()
	runtimeHandler, err := transporthttp.NewRuntimeHandler(
		transporthttp.RuntimeServices{
			Model:            transporthttp.NewHandler(services.Models),
			Sessions:         services.Sessions,
			Runs:             services.Runs,
			AgentBudget:      services.Config.AgentBudget(),
			SettledRetention: services.Config.SettledRetention(),
		},
	)
	if err != nil {
		return fmt.Errorf("build HTTP handler: %w", err)
	}
	readiness := &readinessState{}
	handler := newServerHandler(readiness, config.BearerToken, runtimeHandler)
	handler = auditHTTP(paths.LogsDir, handler)
	server := &http.Server{
		Addr: config.Address, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	listener, err := net.Listen("tcp", config.Address)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", config.Address, err)
	}
	defer listener.Close()
	tasks := make([]backgroundTask, 0, services.Config.Scheduler.Workers+1)
	for index := 0; index < services.Config.Scheduler.Workers; index++ {
		workerID := fmt.Sprintf("sn-server-%d-%d", os.Getpid(), index+1)
		tasks = append(tasks, backgroundTask{
			Name: workerID,
			Run: func(taskContext context.Context) error {
				return services.Runs.Worker(
					taskContext, workerID, services.Config.PollInterval(),
				)
			},
		})
	}
	reaperOptions := runtime.ReaperOptions{
		Interval:               services.Config.ReaperInterval(),
		PausedTTL:              services.Config.ReaperPausedTTL(),
		NeedsReconciliationTTL: services.Config.ReaperNeedsReconciliationTTL(),
	}
	if reaperOptions.Interval > 0 {
		tasks = append(tasks, backgroundTask{
			Name: "reaper",
			Run: func(taskContext context.Context) error {
				return services.Runs.RunReaper(taskContext, reaperOptions)
			},
		})
	}
	log.Printf(
		"listening on %s with %d worker(s)",
		listener.Addr(), services.Config.Scheduler.Workers,
	)
	return superviseServer(ctx, serverLifecycle{
		Readiness: readiness,
		Tasks:     tasks,
		Serve: func() error {
			return server.Serve(listener)
		},
		Shutdown:        server.Shutdown,
		ShutdownTimeout: 10 * time.Second,
	})
}

func validateServerArgs(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("sn-server does not accept command-line arguments")
	}
	return nil
}

func requireActivationReady(paths layout.Paths) error {
	return activationgate.RequireOpen(paths.StateDir)
}

func loadServerConfig(getenv func(string) string) (serverConfig, error) {
	config := serverConfig{
		Address:     strings.TrimSpace(getenv("HTTP_ADDR")),
		BearerToken: strings.TrimSpace(getenv("SN_SERVER_TOKEN")),
	}
	if config.Address == "" {
		config.Address = "127.0.0.1:8080"
	}
	host, _, err := net.SplitHostPort(config.Address)
	if err != nil {
		return serverConfig{}, fmt.Errorf("HTTP_ADDR must be host:port: %w", err)
	}
	if !loopbackHost(host) && config.BearerToken == "" {
		return serverConfig{}, fmt.Errorf(
			"SN_SERVER_TOKEN is required when HTTP_ADDR is not loopback",
		)
	}
	return config, nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func bearerAuth(token string, next http.Handler) http.Handler {
	expected := []byte("Bearer " + token)
	return http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		provided := []byte(request.Header.Get("Authorization"))
		if len(provided) != len(expected) ||
			subtle.ConstantTimeCompare(provided, expected) != 1 {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("Cache-Control", "no-store")
			writer.Header().Set("X-Content-Type-Options", "nosniff")
			writer.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"error": &contract.RuntimeError{
					Code:    contract.ErrorAuthenticationFailed,
					Phase:   contract.PhaseTransport,
					Message: "bearer authentication failed",
				},
			})
			return
		}
		next.ServeHTTP(writer, request)
	})
}
