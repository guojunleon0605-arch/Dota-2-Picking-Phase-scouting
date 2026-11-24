package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"dotascouter/internal/api"
	"dotascouter/internal/config"
	"dotascouter/internal/db"
	"dotascouter/internal/gsi"
	"dotascouter/internal/logwatch"
	"dotascouter/internal/platform"
	"dotascouter/internal/server"
)

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg, err := config.NewManager()
	if err != nil {
		log.Fatalf("init config: %v", err)
	}

	store, err := db.Open(cfg.DBPath())
	if err != nil {
		log.Fatalf("open sqlite db: %v", err)
	}
	defer store.Close()

	scouter := api.NewScouter(cfg, store)

	// Preload hero constants in background so startup is instant
	go func() {
		bgCtx, bgCancel := context.WithTimeout(ctx, 15*time.Second)
		defer bgCancel()
		_ = scouter.OpenDota.EnsureHeroesLoaded(bgCtx, cfg.Get().OpenDotaAPIKey)
	}()

	var appSrv *server.AppServer
	settings := cfg.Get()

	dotaRoot := gsi.DetectDota2Dir(settings.Dota2Path)
	watcher := logwatch.NewWatcher(dotaRoot, func(ids []int64, source string) {
		if appSrv != nil {
			_, _ = appSrv.TriggerPlayersScout(ctx, ids, source, false)
		}
	})
	go watcher.Run(ctx)

	gsiSrv := gsi.NewServer(
		settings.GSIPort,
		// Live Picking Phase handler (Hero Selection / Strategy Time):
		// OpenDota does not have live matches until post-game, so extract 10 Steam IDs
		// from GSI team payloads or local server_log.txt / console.log immediately.
		func(gameState string, matchID int64, gsiAccountIDs []int64) {
			if appSrv == nil {
				return
			}
			appSrv.HandlePickingPhaseTrigger(ctx, gameState, matchID, gsiAccountIDs)
		},
		// Post-Game handler: once match ends, OpenDota indexes the completed match roster.
		func(matchID int64) {
			if appSrv != nil {
				_, _ = appSrv.TriggerMatchScout(ctx, matchID, "GSI Post-Game", false)
			}
		},
		func(st gsi.StateSnapshot) {
			if appSrv != nil {
				appSrv.NotifyGSIStatus(st)
			}
		},
	)
	_ = gsiSrv.Start()
	defer func() {
		shCtx, shCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer shCancel()
		_ = gsiSrv.Shutdown(shCtx)
	}()

	// Bind ephemeral localhost port for the embedded UI
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatalf("listen local ui: %v", err)
	}
	addr := ln.Addr().String()
	appURL := fmt.Sprintf("http://%s", addr)

	appSrv = server.NewAppServer(cfg, store, scouter, gsiSrv, watcher, cancel)
	handler, err := appSrv.Handler()
	if err != nil {
		log.Fatalf("init web handler: %v", err)
	}

	httpSrv := &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 90 * time.Second,
	}

	go func() {
		_ = httpSrv.Serve(ln)
	}()
	go appSrv.RunAutoSyncLoop(ctx)

	// Launch native frameless Edge/Chrome app window (fallback to default browser)
	browserCmd, _ := platform.LaunchAppWindow(appURL)
	if browserCmd != nil {
		go func() {
			_ = browserCmd.Wait()
			time.Sleep(500 * time.Millisecond)
			cancel()
		}()
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case <-ctx.Done():
	case <-sigCh:
		cancel()
	}

	shCtx, shCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer shCancel()
	_ = httpSrv.Shutdown(shCtx)
}
