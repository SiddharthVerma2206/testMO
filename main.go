package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// Version is stamped at build time: -ldflags "-X main.Version=0.1.0".
var Version = "dev"

func main() {
	configPath := flag.String("config", "/etc/testmo/agent.yaml", "path to agent.yaml")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := LoadAgentConfig(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	chain, err := LoadChainConfig(cfg.ChainConfig)
	if err != nil {
		log.Fatalf("chain config: %v", err)
	}

	srv := NewServer(cfg, chain)

	// Cancelled on SIGINT/SIGTERM, which stops the prober and starts shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if srv.prober != nil {
		go srv.prober.Run(ctx)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	logStartup(cfg, chain, srv)

	serveErr := make(chan error, 1)
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serveErr <- err
		}
	}()

	select {
	case err := <-serveErr:
		log.Fatalf("listen on %s: %v", cfg.Listen, err)
	case <-ctx.Done():
		log.Print("signal received, shutting down")
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func logStartup(cfg *AgentConfig, chain *ChainConfig, srv *Server) {
	log.Printf("testmo-agent %s listening on %s", Version, cfg.Listen)
	log.Printf("prometheus: %s", cfg.PrometheusURL)

	// Worth a line at startup: a wrong or missing origin leaves no server-side
	// trace at all — it surfaces only as a CORS error in someone's browser.
	if len(cfg.AllowedOrigins) == 0 {
		log.Print("cors: any origin (allowed_origins is unset)")
	} else {
		log.Printf("cors: %s", strings.Join(cfg.AllowedOrigins, ", "))
	}

	if chain == nil {
		log.Printf("chain: no config loaded (%s) — serving system metrics only", cfg.ChainConfig)
		return
	}
	log.Printf("chain: %s (%s), %d metrics", chain.ChainType, chain.ClientName, len(srv.chainMetrics))
	if srv.prober == nil {
		log.Print("probes: none configured")
		return
	}
	log.Printf("probes: %d calls against %s every %s",
		len(chain.LocalCalls), srv.prober.url, srv.prober.interval)
}
