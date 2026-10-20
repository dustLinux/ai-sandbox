// Command sandboxd is the per-VPS MCP sandbox server.
//
// It exposes one MCP endpoint (Streamable HTTP), a public /health endpoint and
// a minimal OAuth surface, and it streams the workspace to the pool when the
// sandbox is released.
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

	"aisandbox/internal/sandbox"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	check := flag.Bool("check", false, "validate the configuration and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("ai-sandbox sandboxd 0.1.0")
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("sandboxd: ")

	cfg, err := sandbox.LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *check {
		fmt.Printf("ok: id=%s root=%s archive=%s idle=%s\n", cfg.SandboxID, cfg.Root, cfg.ArchiveMode, cfg.IdleTimeout)
		return
	}
	srv, err := sandbox.New(cfg)
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	httpServer := &http.Server{
		Addr:              cfg.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 << 10,
	}
	go func() {
		log.Printf("listening on %s (id=%s root=%s archive=%s idle=%s)", cfg.Listen, cfg.SandboxID, cfg.Root, cfg.ArchiveMode, cfg.IdleTimeout)
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = httpServer.Shutdown(ctx)
}
