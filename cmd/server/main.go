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

	"agent-arch/internal/agent"
	"agent-arch/internal/config"
	"agent-arch/internal/transport"
)

func main() {
	ctx := context.Background()

	cfg, err := config.Load(ctx, "configs/config.yaml")
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	service, err := agent.NewService(ctx, cfg, "configs/personas")
	if err != nil {
		log.Fatalf("build service: %v", err)
	}

	server := &http.Server{
		Addr:              cfg.Server.HTTPAddr,
		Handler:           transport.NewHTTPHandler(service),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s", cfg.Server.HTTPAddr)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
}
