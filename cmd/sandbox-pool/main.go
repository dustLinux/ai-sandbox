// Command sandbox-pool is the control box of the AI sandbox fleet.
//
// It owns the Vibe Hosting accounts, provisions sandbox servers, deploys the
// sandbox binary over SSH, keeps leased sandboxes awake, receives workspace
// archives and uploads them to Google Drive through rclone.
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

	"aisandbox/internal/pool"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	check := flag.Bool("check", false, "validate the configuration and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println("ai-sandbox pool 0.1.0")
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)
	log.SetPrefix("pool: ")

	cfg, err := pool.LoadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	if *check {
		fmt.Printf("ok: public=%s state=%s remote=%s quota=%d\n", cfg.PublicURL, cfg.StateDir, cfg.ArchiveRemote, cfg.QuotaBytes)
		return
	}
	p, err := pool.New(cfg)
	if err != nil {
		log.Fatalf("start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.Start(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Printf("listening on %s (public=%s)", cfg.Listen, cfg.PublicURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("listen: %v", err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Print("shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}
