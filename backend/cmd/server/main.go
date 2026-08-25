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

	http.Handle("/webhook/payment-failed", ingest.NewHandler(client))

	fmt.Println("server up")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		fmt.Fprintln(os.Stderr, "server stopped:", err)
		os.Exit(1)
	}
}
