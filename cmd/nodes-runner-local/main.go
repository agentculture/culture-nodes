// Command nodes-runner-local serves the registered local runner over the
// runner protocol. It is a separate process from the authoritative service.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/agentculture/culture-nodes/internal/runners/local"
	"github.com/agentculture/culture-nodes/internal/runners/runnerservice"
)

func main() {
	listen := flag.String("listen", ":8092", "runner listen address")
	stateDir := flag.String("state-dir", "", "durable operation status directory")
	flag.Parse()
	secret := os.Getenv("NODES_RUNNER_SECRET")
	if secret == "" {
		fatal(errors.New("NODES_RUNNER_SECRET is required"))
	}
	if *stateDir == "" {
		fatal(errors.New("--state-dir is required"))
	}
	runner, err := local.New(local.Config{})
	if err != nil {
		fatal(err)
	}
	store, err := runnerservice.NewFileStore(*stateDir)
	if err != nil {
		fatal(err)
	}
	svc, err := runnerservice.New(runnerservice.Config{Runner: runner, Store: store, Secret: secret})
	if err != nil {
		fatal(err)
	}
	defer svc.Close()
	server := &http.Server{Addr: *listen, Handler: handler(svc.Handler()), ReadHeaderTimeout: 10 * time.Second}
	fmt.Fprintf(os.Stdout, "runner=%s revision=%s image_digest=%s listen=%s\n", local.RunnerName, local.Revision, local.ImageDigest, *listen)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errs := make(chan error, 1)
	go func() { errs <- server.ListenAndServe() }()
	select {
	case <-ctx.Done():
	case err = <-errs:
		if !errors.Is(err, http.ErrServerClosed) {
			fatal(err)
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdown); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}

func handler(protocol http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/v1/", protocol)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	return mux
}

func fatal(err error) { fmt.Fprintln(os.Stderr, "nodes-runner-local:", err); os.Exit(1) }
