// Command runnersampler is a throwaway fault-test fixture, not a
// culture-nodes product binary. It lives under testdata/ so Go's tooling
// leaves it out of `go build ./...`, `go vet ./...` and `go test ./...`
// package discovery.
//
// tests/fault/runnerasync_fault_test.go `go build`s this package and execs
// two copies of it against one ephemeral Postgres and one real HTTP runner
// service, so task t9's claim -- "a worker killed mid-operation strands
// nothing: the surviving worker resumes tracking after handoff" -- is proven
// with an actual SIGKILL of an actual process rather than simulated inside
// one test binary.
//
// It runs the REAL internal/worker loop: the same Tick that claims ready work
// and dispatches it, and the same SampleRunnerOperations that reads parked
// runner operations out of durable state. Nothing about the recovery path is
// special-cased for the test; the survivor process is configured exactly like
// the victim was.
//
// The victim and the survivor differ in one env var. The victim sets
// SAMPLER_PARKED_FLAG_FILE and SAMPLER_HANG_AFTER_PARK, which make it touch a
// flag file the moment it has parked an operation and then block forever --
// so the parent test can land its kill at a precisely known point: after the
// operation was dispatched and parked, before any status was ever sampled.
//
// Required env vars:
//
//	SAMPLER_DB_URL             postgres connection string
//	SAMPLER_NAMESPACE_ID       namespace to work in
//	SAMPLER_WORKER_ID          lease owner / worker identity
//	SAMPLER_RUNNER_REF         registry name the code node's `uses` declares
//	SAMPLER_RUNNER_ENDPOINT    the runner service's base URL
//	SAMPLER_RUNNER_DIGEST      the pinned execution-environment digest
//	SAMPLER_SECRET_REF         credential reference the registry stores
//	SAMPLER_SECRET             material the resolver returns for that ref
//	SAMPLER_ACTOR_ID           registered actor the evidence is attributed to
//	SAMPLER_RUNNER_NAME        logical runner name stamped on the operation
//	SAMPLER_POLL_MS            sampling cadence
//	SAMPLER_IDLE_TIMEOUT_MS    exit after this long with nothing to do
//
// Optional env vars:
//
//	SAMPLER_PARKED_FLAG_FILE   touched once an operation has parked
//	SAMPLER_HANG_AFTER_PARK    "1" to block forever after parking one
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/agentculture/culture-nodes/internal/engine"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/store/postgres"
	"github.com/agentculture/culture-nodes/internal/worker"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "runnersampler: %v\n", err)
		os.Exit(1)
	}
}

type config struct {
	dbURL, namespaceID, workerID          string
	runnerRef, endpoint, digest           string
	secretRef, secret, actorID, runnerNam string
	poll, idleTimeout                     time.Duration
	flagFile                              string
	hangAfterPark                         bool
}

func loadConfig() (config, error) {
	var cfg config
	var err error
	for _, field := range []struct {
		name string
		dest *string
	}{
		{"SAMPLER_DB_URL", &cfg.dbURL},
		{"SAMPLER_NAMESPACE_ID", &cfg.namespaceID},
		{"SAMPLER_WORKER_ID", &cfg.workerID},
		{"SAMPLER_RUNNER_REF", &cfg.runnerRef},
		{"SAMPLER_RUNNER_ENDPOINT", &cfg.endpoint},
		{"SAMPLER_RUNNER_DIGEST", &cfg.digest},
		{"SAMPLER_SECRET_REF", &cfg.secretRef},
		{"SAMPLER_SECRET", &cfg.secret},
		{"SAMPLER_ACTOR_ID", &cfg.actorID},
		{"SAMPLER_RUNNER_NAME", &cfg.runnerNam},
	} {
		if *field.dest, err = requireEnv(field.name); err != nil {
			return cfg, err
		}
	}
	pollMS, err := requireEnvInt("SAMPLER_POLL_MS")
	if err != nil {
		return cfg, err
	}
	idleMS, err := requireEnvInt("SAMPLER_IDLE_TIMEOUT_MS")
	if err != nil {
		return cfg, err
	}
	cfg.poll = time.Duration(pollMS) * time.Millisecond
	cfg.idleTimeout = time.Duration(idleMS) * time.Millisecond
	cfg.flagFile = os.Getenv("SAMPLER_PARKED_FLAG_FILE")
	cfg.hangAfterPark = os.Getenv("SAMPLER_HANG_AFTER_PARK") == "1"
	return cfg, nil
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}

	ctx := context.Background()
	s, err := postgres.Connect(ctx, cfg.dbURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer s.Close()

	wk, err := buildWorker(s, cfg)
	if err != nil {
		return err
	}
	return pollLoop(ctx, s, wk, cfg)
}

// buildWorker wires the real worker exactly as a deployment that dispatches
// its code to a runner service would: a registry holding one ServiceIdentity,
// a protocol client over a secret source, and no in-process CodeRunner at all.
func buildWorker(s *postgres.Store, cfg config) (*worker.Worker, error) {
	eng, err := postgres.NewEngine(s, cfg.namespaceID, engine.WithRetryDelays(0, 0))
	if err != nil {
		return nil, fmt.Errorf("NewEngine: %w", err)
	}

	registry := runners.NewFunctionRegistry()
	if err := registry.RegisterService(cfg.runnerRef, runners.ServiceIdentity{
		Endpoint:               cfg.endpoint,
		ImageDigest:            cfg.digest,
		SecretRef:              cfg.secretRef,
		AllowInsecureTransport: true,
	}); err != nil {
		return nil, fmt.Errorf("RegisterService: %w", err)
	}
	client, err := runners.NewProtocolClient(runners.StaticSecrets{cfg.secretRef: cfg.secret})
	if err != nil {
		return nil, fmt.Errorf("NewProtocolClient: %w", err)
	}

	wk, err := worker.New(s, eng, worker.Options{
		WorkerID:          cfg.workerID,
		NamespaceID:       cfg.namespaceID,
		ClaimBatch:        4,
		LeaseDuration:     30 * time.Second,
		HeartbeatInterval: 5 * time.Second,
		PollInterval:      cfg.poll,
		CodeRunnerName:    cfg.runnerNam,
		CodeRunnerActorID: cfg.actorID,
		RunnerService: worker.RunnerServiceOptions{
			Registry:     registry,
			Client:       client,
			PollInterval: cfg.poll,
			SampleBatch:  8,
			// No callback is offered: this worker has no externally reachable
			// URL, which is a fully conformant deployment. Recovery here is
			// therefore proven on POLLING ALONE -- the thing the protocol
			// document says must be sufficient.
		},
		OnError: func(err error) {
			fmt.Printf("worker %s: %v\n", cfg.workerID, err)
		},
	})
	if err != nil {
		return nil, fmt.Errorf("worker.New: %w", err)
	}
	return wk, nil
}

// pollLoop drives the real worker until there is nothing left to do, writing
// the coordination flag once an operation has parked.
func pollLoop(ctx context.Context, s *postgres.Store, wk *worker.Worker, cfg config) error {
	lastProgress := time.Now()
	flagWritten := false

	for {
		if time.Since(lastProgress) > cfg.idleTimeout {
			fmt.Printf("worker %s: idle timeout after %s, exiting\n", cfg.workerID, cfg.idleTimeout)
			return nil
		}

		dispatched, err := wk.Tick(ctx)
		if err != nil {
			return fmt.Errorf("Tick: %w", err)
		}
		sampled, err := wk.SampleRunnerOperations(ctx)
		if err != nil {
			return fmt.Errorf("SampleRunnerOperations: %w", err)
		}
		if dispatched > 0 || sampled > 0 {
			lastProgress = time.Now()
		}

		parked, err := parkedCount(ctx, s, cfg.namespaceID)
		if err != nil {
			return err
		}
		if parked > 0 && !flagWritten && cfg.flagFile != "" {
			if err := os.WriteFile(cfg.flagFile, []byte(strconv.Itoa(parked)+"\n"), 0o644); err != nil {
				return fmt.Errorf("write parked flag file: %w", err)
			}
			flagWritten = true
			fmt.Printf("worker %s: parked %d runner operation(s)\n", cfg.workerID, parked)
		}
		if parked > 0 && cfg.hangAfterPark {
			// Block forever: this process is now "mid-operation" and the
			// parent test is about to SIGKILL it. Everything needed to finish
			// the operation is already durable, and this process holds no
			// lease on any of it.
			fmt.Printf("worker %s: hanging after park, awaiting kill\n", cfg.workerID)
			select {}
		}

		time.Sleep(cfg.poll)
	}
}

func parkedCount(ctx context.Context, s *postgres.Store, namespaceID string) (int, error) {
	var n int
	if err := s.Pool().QueryRow(ctx,
		`SELECT count(*) FROM runner_invocations WHERE namespace_id = $1 AND state = 'waiting_external'`,
		namespaceID).Scan(&n); err != nil {
		return 0, fmt.Errorf("count parked runner operations: %w", err)
	}
	return n, nil
}

func requireEnv(name string) (string, error) {
	v := os.Getenv(name)
	if v == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return v, nil
}

func requireEnvInt(name string) (int, error) {
	v, err := requireEnv(name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return n, nil
}
