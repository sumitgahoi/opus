package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"opus/internal/auth"
	"opus/internal/inventory"
	"opus/internal/web"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(2)
		}
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	configFlag := flag.String("config", "", "configuration file")
	rootFlag := flag.String("root", "", "music folder to inventory")
	dbFlag := flag.String("db", "inventory.db", "SQLite inventory database")
	addr := flag.String("addr", "0.0.0.0:80", "listen address")
	scanInterval := flag.Duration("scan-interval", 15*time.Minute, "rescan period; 0 disables the timer")
	flag.Parse()
	cfg, err := resolveConfig(*configFlag, *rootFlag)
	if err != nil {
		return err
	}
	root, err := inventory.CleanRoot(cfg.MusicRoot)
	if err != nil {
		return err
	}
	store, err := inventory.Open(*dbFlag)
	if err != nil {
		return err
	}
	defer store.Close()

	var authSvc *auth.Service
	if *configFlag != "" {
		sessions := auth.NewSessions(store.DB())
		if err := sessions.EnsureSchema(); err != nil {
			return err
		}
		authSvc = auth.New(sessions, cfg.Auth)
		if !authSvc.Enabled() {
			return errors.New("sign-in is not configured")
		}
	}

	handler := web.New(root, store, authSvc)
	handler.Start(*scanInterval)
	server := &http.Server{
		Addr:              *addr,
		Handler:           handler.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on http://%s (health at /health)", *addr)
	return server.ListenAndServe()
}
