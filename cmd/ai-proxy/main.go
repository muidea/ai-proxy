package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ai-proxy/internal/archive"
	"ai-proxy/internal/config"
	"ai-proxy/internal/proxy"
	"ai-proxy/internal/stats"
)

func main() {
	configPath := flag.String("config", os.Getenv("AI_PROXY_CONFIG"), "config file path")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	recorder := stats.NewCSVRecorder(cfg.UsageFile)
	interactionRecorder, err := archive.NewRecorder(cfg.InteractionDir, cfg.InteractionRetention)
	if err != nil {
		log.Fatalf("init interaction recorder: %v", err)
	}
	handler := proxy.NewHandler(cfg, recorder, interactionRecorder)
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		fmt.Printf("ai-proxy listening on %s, usage file: %s, interactions: %s\n", cfg.ListenAddr, cfg.UsageFile, cfg.InteractionDir)
		errCh <- server.ListenAndServe()
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case sig := <-stop:
		fmt.Printf("received %s, shutting down\n", sig)
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("server error: %v", err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Fatalf("shutdown: %v", err)
	}
}
