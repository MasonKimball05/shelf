// Command shelf streams my media library from the desktop to Media Player over
// Tailscale. It's run and restarted by homebase.
//
//	shelf -config shelf.json
//
// Secrets come from the environment (homebase's env_file), never the config:
//
//	SHELF_TOKEN   bearer token Media Player sends to the API
//	SHELF_SECRET  key for signing stream links (32+ characters)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/MasonKimball05/shelf/internal/library"
	"github.com/MasonKimball05/shelf/internal/progress"
	"github.com/MasonKimball05/shelf/internal/server"
	"github.com/MasonKimball05/shelf/internal/thumbs"
)

type config struct {
	Listen        string         `json:"listen"`
	PublicURL     string         `json:"public_url"`
	Roots         []library.Root `json:"roots"`
	LinkTTLHours  int            `json:"link_ttl_hours"`
	RescanMinutes int            `json:"rescan_minutes"`
	// DataDir holds the thumbnail cache and saved positions. Default: a "data"
	// folder next to the config file.
	DataDir string `json:"data_dir"`
	// FFmpeg is the path to ffmpeg, for thumbnails. Default: found on PATH.
	FFmpeg string `json:"ffmpeg"`
}

func loadConfig(path string) (config, error) {
	c := config{Listen: "127.0.0.1:8095", LinkTTLHours: 12, RescanMinutes: 30}
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(c.Roots) == 0 {
		return c, errors.New("config has no roots")
	}
	if c.PublicURL == "" {
		c.PublicURL = "http://" + c.Listen
	}
	if c.DataDir == "" {
		c.DataDir = filepath.Join(filepath.Dir(path), "data")
	}
	return c, nil
}

func main() {
	path := flag.String("config", "shelf.json", "config file")
	flag.Parse()

	cfg, err := loadConfig(*path)
	if err != nil {
		log.Fatal(err)
	}
	token, secret := os.Getenv("SHELF_TOKEN"), os.Getenv("SHELF_SECRET")
	if len(token) < 24 || len(secret) < 32 {
		log.Fatal("set SHELF_TOKEN (24+ chars) and SHELF_SECRET (32+ chars), e.g. in homebase's env_file")
	}

	lib := library.New(cfg.Roots)
	start := time.Now()
	_ = lib.Scan()
	files, _ := lib.Stats()
	log.Printf("indexed %d files in %s", files, time.Since(start).Round(time.Millisecond))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Pick up new downloads without a restart.
	go func() {
		t := time.NewTicker(time.Duration(cfg.RescanMinutes) * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = lib.Scan()
			}
		}
	}()

	signer := server.NewSigner([]byte(secret), time.Duration(cfg.LinkTTLHours)*time.Hour)
	api := server.New(lib, signer, token, cfg.PublicURL)

	// Both extras are optional: shelf still streams without them.
	if maker, err := thumbs.New(filepath.Join(cfg.DataDir, "thumbs"), cfg.FFmpeg); err != nil {
		log.Printf("thumbnails off: %v", err)
	} else {
		api.SetThumbnails(maker)
	}
	store, err := progress.Open(filepath.Join(cfg.DataDir, "progress.json"))
	if err != nil {
		log.Printf("saved positions: %v (starting fresh)", err)
	}
	api.SetProgress(store)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		// No WriteTimeout: a movie stream legitimately runs for hours.
		IdleTimeout: 2 * time.Minute,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()

	log.Printf("shelf listening on %s (links: %s)", cfg.Listen, cfg.PublicURL)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
