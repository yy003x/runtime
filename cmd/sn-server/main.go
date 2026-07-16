package main

import (
	"context"
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

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/layout"
	"agent-runtime/internal/transport"
)

type serverConfig struct {
	Address     string
	BearerToken string
}

func main() {
	paths, err := layout.Resolve()
	if err != nil {
		log.Fatalf("resolve runtime home: %v", err)
	}
	if err := paths.Ensure(); err != nil {
		log.Fatalf("prepare runtime home: %v", err)
	}
	service := agentrun.New(paths.Home)
	config, err := loadServerConfig(os.Getenv)
	if err != nil {
		log.Fatalf("server config: %v", err)
	}

	server := &http.Server{
		Addr:              config.Address,
		Handler:           transport.NewHTTPHandlerWithOptions(service, transport.HTTPOptions{BearerToken: config.BearerToken}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
	}

	go func() {
		log.Printf("listening on %s", config.Address)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
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
		return serverConfig{}, fmt.Errorf("SN_SERVER_TOKEN is required when HTTP_ADDR is not loopback")
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
