package main

import (
	"errors"
	"log"
	"net/http"
	"os"

	"github.com/hkjx79majg/signalwatch/internal/server"
)

func main() {
	addr := os.Getenv("SIGNALWATCH_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	log.Printf("SignalWatch listening on %s", addr)
	if err := http.ListenAndServe(addr, server.Handler()); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
