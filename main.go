package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to configuration file")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	if err := cfg.Validate(); err != nil {
		log.Fatalf("invalid config: %v", err)
	}

	srv := NewServer(cfg)

	log.Printf("local-fusion-gateway starting on %s", cfg.Listen)
	log.Printf("virtual model: %s", cfg.VirtualModel)
	log.Printf("panel models: %d", len(cfg.Panel))

	if err := http.ListenAndServe(cfg.Listen, srv.Handler()); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
