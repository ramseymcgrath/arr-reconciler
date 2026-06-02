package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ramseymcgrath/arr-reconciler/internal/config"
	"github.com/ramseymcgrath/arr-reconciler/internal/reconcile"
)

func main() {
	var (
		configPath = flag.String("config", "/etc/arr-reconciler/config.json", "path to config file")
		interval   = flag.Duration("interval", 0, "if >0, run on this interval as a daemon; otherwise run once and exit")
		dryRun     = flag.Bool("dry-run", false, "override config: report actions without performing them")
		jitter     = flag.Duration("jitter", 0, "random startup delay up to this duration (daemon mode) to avoid thundering herd")
	)
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Error("load config", "err", err)
		os.Exit(1)
	}
	if *dryRun {
		cfg.DryRun = true
	}

	log.Info("starting arr-reconciler",
		"instances", len(cfg.Instances),
		"dry_run", cfg.DryRun,
		"delete_enabled", cfg.Safety.DeleteEnabled,
		"interval", interval.String(),
	)

	engine := reconcile.New(cfg, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	runOnce := func() {
		runCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		defer cancel()
		rep, err := engine.Run(runCtx)
		if err != nil {
			log.Error("run failed", "err", err)
			return
		}
		fmt.Println(rep.Summary())
	}

	if *interval <= 0 {
		runOnce()
		return
	}

	if *jitter > 0 {
		d := time.Duration(time.Now().UnixNano()) % *jitter
		log.Info("startup jitter", "delay", d.String())
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return
		}
	}

	runOnce() // immediate first pass
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return
		case <-ticker.C:
			runOnce()
		}
	}
}
