// Command server runs the common-service HTTP API:
//
//   - sandboxed code execution streamed over server-sent events
//   - Markdown to PDF rendering through headless Chrome
//   - real-time collaborative code rooms over WebSocket
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nirmalparmar/common-service/internal/collab"
	"github.com/nirmalparmar/common-service/internal/compiler"
	"github.com/nirmalparmar/common-service/internal/config"
	"github.com/nirmalparmar/common-service/internal/httpx"
	"github.com/nirmalparmar/common-service/internal/mdpdf"
	"github.com/nirmalparmar/common-service/internal/sandbox"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-healthcheck" {
		os.Exit(healthcheck())
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	origins := httpx.NewOrigins(cfg.AllowedOrigins, cfg.RequireOrigin)
	mux := http.NewServeMux()
	features := map[string]bool{}

	var comp *compiler.Service
	if cfg.Compiler.Enabled {
		box, err := sandbox.New(sandbox.Options{
			Backend:     cfg.Compiler.Sandbox,
			NsjailPath:  cfg.Compiler.NsjailPath,
			DockerImage: cfg.Compiler.DockerImage,
			Slots:       cfg.Compiler.MaxConcurrent,
		})
		if err != nil {
			return err
		}
		if cfg.Compiler.Sandbox == "unsafe" {
			log.Warn("SANDBOX=unsafe: submitted code runs WITHOUT isolation. Never expose this server publicly.")
		}
		comp = compiler.New(cfg.Compiler, box, log)
		probeCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		comp.Probe(probeCtx)
		cancel()
		limit := httpx.NewRateLimiter(cfg.Compiler.RatePerMinute, cfg.Compiler.RateBurst).Limit
		comp.Routes(mux, limit)
		features["compiler"] = true
		log.Info("compiler ready", "sandbox", box.Name(), "languages", len(comp.Languages()))
	}

	if cfg.PDF.Enabled {
		r := mdpdf.NewRenderer(cfg.PDF, log)
		defer r.Close()
		limit := httpx.NewRateLimiter(cfg.PDF.RatePerMinute, cfg.PDF.RateBurst).Limit
		mdpdf.Routes(mux, r, cfg.PDF, limit)
		features["markdown_pdf"] = true
	}

	if cfg.Collab.Enabled {
		hub, err := collab.NewHub(cfg.Collab, cfg.DataDir, comp, origins.Hosts(), log)
		if err != nil {
			return err
		}
		defer hub.Close()
		hub.Routes(mux,
			httpx.NewRateLimiter(cfg.Collab.CreatePerMinute, max(cfg.Collab.CreatePerMinute/2, 3)).Limit,
			httpx.NewRateLimiter(cfg.Collab.ConnectPerMinute, max(cfg.Collab.ConnectPerMinute/3, 10)).Limit,
		)
		features["collab"] = true
	}

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, http.StatusOK, map[string]any{"status": "ok", "features": features})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		httpx.Error(w, http.StatusNotFound, "not_found", "no such endpoint")
	})

	handler := httpx.Chain(mux,
		httpx.RealIP(cfg.TrustedProxies),
		httpx.Logger(log),
		httpx.Recover(log),
		httpx.SecurityHeaders,
		origins.CORS,
	)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      90 * time.Second, // lifted per-response for streams
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
		BaseContext:       func(_ net.Listener) context.Context { return ctx },
	}

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.Addr, "env", cfg.Env)
		errc <- srv.ListenAndServe()
	}()

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			log.Warn("forced shutdown", "err", err)
		}
	}
	return nil
}

// healthcheck probes the local /healthz endpoint (used by Docker HEALTHCHECK).
func healthcheck() int {
	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
