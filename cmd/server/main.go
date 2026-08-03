package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"interest-memory/internal/config"
	"interest-memory/internal/httpapi"
	"interest-memory/internal/llm"
	"interest-memory/internal/service"
	"interest-memory/internal/store"
	"interest-memory/internal/vec"
	"interest-memory/internal/worker"

	"github.com/djasdh/my-agent-core/gateway"
)

func main() {
	configPath := flag.String("config", "", "path to YAML config file (default: built-in defaults)")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	// Register sqlite-vec auto-extension BEFORE opening the store connection.
	vec.Init()
	st, err := store.Open(cfg.Server.DBPath)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.Close()

	// Vector index: prefer sqlite-vec, degrade to keyword fallback.
	var vi vec.VectorIndex
	if sv, err := vec.NewSQLiteVec(st.DB(), cfg.Embedding.Dimensions); err == nil && sv.Available() {
		vi = sv
		log.Printf("vec: sqlite-vec available")
	} else {
		fv, ferr := vec.NewFallback(st.DB())
		if ferr != nil {
			log.Fatalf("vec fallback: %v", ferr)
		}
		vi = fv
		log.Printf("vec: sqlite-vec unavailable, using keyword fallback")
	}
	defer vi.Close()

	llmClient := llm.New(cfg.LLM)
	embedder := llm.NewEmbedder(cfg.Embedding)

	svc := service.New(cfg, st, vi, llmClient, embedder)
	wk := worker.New(svc, st, cfg.Worker.JobTimeout)
	defer wk.Close()

	// Stage 5: the my-agent-core gateway is the single HTTP layer. It provides
	// /api/chat (SSE), /api/upload, CORS and /api/health. No agent factory is
	// registered — /api/chat routes return "no factory registered"; the memory
	// endpoints are mounted via Server.Register.
	gw := gateway.New()
	api := httpapi.NewServer(svc, wk)
	srv := gateway.NewServer(gw)
	srv.Addr = fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	for _, r := range api.Routes() {
		srv.Register(r.Pattern, r.Handler)
	}

	log.Printf("interest-memory listening on %s (db=%s)", srv.Addr, cfg.Server.DBPath)

	// gateway.Server exposes no Shutdown; on SIGINT/SIGTERM we return from
	// main so the deferred store/vec/worker Close calls run.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	case <-ctx.Done():
		log.Printf("shutting down")
	}
}
