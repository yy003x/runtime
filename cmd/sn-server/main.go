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
	"sync"
	"syscall"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/activation"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	transporthttp "github.com/yy003x/runtime/transport/http"
)

type serverConfig struct {
	Address     string
	BearerToken string
}

var fixedNamespaces = []string{
	"profile", "session", "tmux", "agent", "run", "server", "help", "version",
}

func main() {
	if err := validateServerArgs(os.Args[1:]); err != nil {
		log.Fatalf("arguments: %v", err)
	}
	paths, err := layout.Resolve()
	if err != nil {
		log.Fatalf("resolve runtime home: %v", err)
	}
	if err := requireActivationReady(paths); err != nil {
		log.Fatalf("activation gate: %v", err)
	}
	config, err := loadServerConfig(os.Getenv)
	if err != nil {
		log.Fatalf("server config: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve working directory: %v", err)
	}
	services, err := runtimebootstrap.LoadServicesWithRunRecovery(
		paths,
		cwd,
		fixedNamespaces...,
	)
	if err != nil {
		log.Fatalf("initialize Runtime vNext: %v", err)
	}
	defer services.Runs.Close()
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
		log.Fatalf("build HTTP handler: %v", err)
	}
	handler := http.Handler(runtimeHandler)
	if config.BearerToken != "" {
		handler = bearerAuth(config.BearerToken, handler)
	}
	server := &http.Server{
		Addr: config.Address, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      0,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}
	workerContext, cancelWorkers := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	for index := 0; index < services.Config.Scheduler.Workers; index++ {
		workers.Add(1)
		go func(worker int) {
			defer workers.Done()
			workerID := fmt.Sprintf("sn-server-%d-%d", os.Getpid(), worker)
			err := services.Runs.Worker(
				workerContext, workerID, services.Config.PollInterval(),
			)
			if err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("worker %s stopped: %v", workerID, err)
			}
		}(index + 1)
	}
	serveDone := make(chan error, 1)
	go func() {
		log.Printf("listening on %s with %d worker(s)", config.Address, services.Config.Scheduler.Workers)
		serveDone <- server.ListenAndServe()
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	var serveErr error
	select {
	case <-stop:
		shutdownContext, cancelShutdown := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		if err := server.Shutdown(shutdownContext); err != nil {
			log.Printf("shutdown HTTP: %v", err)
		}
		cancelShutdown()
	case serveErr = <-serveDone:
	}
	cancelWorkers()
	workers.Wait()
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		log.Fatalf("listen: %v", serveErr)
	}
}

func validateServerArgs(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("sn-server does not accept command-line arguments")
	}
	return nil
}

func requireActivationReady(paths layout.Paths) error {
	return activation.RequireNoGuard(paths.StateDir)
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
