// jev-proxy: an OpenAI-compatible passthrough proxy that scores every final
// text reply with Jev, asynchronously and without touching the response.
package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"jev-proxy/internal/config"
	"jev-proxy/internal/dotenv"
	"jev-proxy/internal/jev"
	"jev-proxy/internal/proxy"
	"jev-proxy/internal/rubric"
	"jev-proxy/internal/scorer"
	"jev-proxy/internal/store"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "calibrate" {
		calibrateMain(os.Args[2:])
		return
	}
	serveMain(os.Args[1:])
}

// boot wires the shared startup for both modes: -env/-config flags, the
// optional .env load (real environment wins over file values), then config
// validation. Calibration must hit exactly the route serving uses.
func boot(fs *flag.FlagSet, args []string) *config.Config {
	envPath := fs.String("env", "", "path to a .env file (default: ./.env, else one beside the config)")
	configPath := fs.String("config", "config.yaml", "path to the YAML config file")
	if err := fs.Parse(args); err != nil {
		os.Exit(2) // flag.ExitOnError pre-exits; belt and braces
	}
	file, err := dotenv.ResolvePath(*envPath, *configPath)
	if err != nil {
		log.Fatalf("jev-proxy: %v", err)
	}
	if err := dotenv.Apply(file); err != nil {
		log.Fatalf("jev-proxy: %v", err)
	}
	if file != "" {
		log.Printf("jev-proxy: loaded env file %s (variables already set in the shell take precedence)", file)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("jev-proxy: %v", err)
	}
	for _, k := range cfg.MissingKeys {
		log.Printf("jev-proxy: WARNING provider key missing: %s (requests to it will fail until set)", k)
	}
	return cfg
}

// newJevClient builds the one evaluate client shared by scoring and calibration.
func newJevClient(cfg *config.Config) *jev.Client {
	return &jev.Client{
		URL:     cfg.Jev.Endpoint,
		APIKey:  cfg.Jev.APIKey(),
		Model:   cfg.Jev.Model,
		HTTP:    &http.Client{Timeout: cfg.Jev.Timeout},
		Backoff: 500 * time.Millisecond,
	}
}

func serveMain(args []string) {
	cfg := boot(flag.NewFlagSet("serve", flag.ExitOnError), args)

	st, err := store.Open(cfg.Log.Path)
	if err != nil {
		log.Fatalf("jev-proxy: %v", err)
	}
	sc := scorer.New(newJevClient(cfg), st, cfg)
	h := proxy.New(cfg, sc, st)

	srv := &http.Server{Addr: cfg.Listen, Handler: h}
	done := make(chan struct{})
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		log.Printf("jev-proxy: shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(done)
	}()

	log.Printf("jev-proxy: listening on %s → upstream %s (jev via %s, rubric %s)",
		cfg.Listen, cfg.Upstream, cfg.Jev.Provider, rubric.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("jev-proxy: %v", err)
	}
	<-done
	sc.Close()
	if err := st.Close(); err != nil {
		log.Printf("jev-proxy: close store: %v", err)
	}
}
