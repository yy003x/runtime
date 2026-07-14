package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/transport"
)

func main() {
	root, err := os.Getwd()
	if err != nil {
		log.Fatalf("resolve runtime root: %v", err)
	}
	if configured := os.Getenv("AGENT_RUNTIME_ROOT"); configured != "" {
		root = configured
	}
	service := agentrun.New(root)
	address := os.Getenv("HTTP_ADDR")
	if address == "" {
		address = ":8080"
	}

	server := &http.Server{
		Addr:              address,
		Handler:           transport.NewHTTPHandler(service),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", address)
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
