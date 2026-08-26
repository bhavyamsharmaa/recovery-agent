package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/bhavyamsharmaa/recovery-agent/internal/decide"
	"github.com/bhavyamsharmaa/recovery-agent/internal/ingest"
)

func main() {
	client, err := decide.NewClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Declared as the interface, not the concrete type, so nothing below this
	// line can reach for an implementation detail. Day 5 changes the constructor
	// on the right and nothing else.
	var attempts ingest.AttemptStore = ingest.NewInMemoryAttemptStore()

	http.Handle("/webhook/payment-failed", ingest.NewHandler(client, attempts))

	fmt.Println("server up")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Fprintln(os.Stderr, "server stopped:", err)
		os.Exit(1)
	}
}
