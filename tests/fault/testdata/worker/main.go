// Command worker is a throwaway fault-test fixture, not a culture-nodes
// product binary. It lives under testdata/ specifically so Go's tooling
// leaves it out of `go build ./...`, `go vet ./...`, and `go test ./...`
// package discovery (testdata directories are always skipped by "...").
// tests/fault/claiming_fault_test.go `go build`s this package into a real
// OS binary and `exec`s two or more copies of it against one ephemeral
// Postgres, so internal/store/postgres's SKIP LOCKED claim and
// fencing-token guard (see claiming.go) can be proven under actual
// process-level concurrency -- including a real SIGKILL -- rather than
// simulated with goroutines inside a single test binary.
//
// The worker repeatedly: reclaims expired leases, claims a batch of ready
// work, "does the work" (a configurable sleep), records an effective
// completion in a test-only results table, then marks the work item
// completed. It exits cleanly once it has seen no claimable/reclaimable
// work for WORKER_IDLE_TIMEOUT_MS.
//
// Required env vars:
//
//	WORKER_DB_URL             postgres connection string
//	WORKER_ID                 lease_owner / results.completed_by value
//	WORKER_LEASE_SECONDS      lease duration passed to ClaimWork (float)
//	WORKER_LIMIT              ClaimWork batch size
//	WORKER_WORK_MS            time to "work" between claim and complete
//	WORKER_IDLE_TIMEOUT_MS    exit after this long with nothing to do
//
// Optional env vars:
//
//	WORKER_CLAIMED_FLAG_FILE  touched (created) the first time this worker
//	                          claims a non-empty batch, so the parent test
//	                          can synchronize a SIGKILL to "after claim,
//	                          before completion" without a race.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/agentculture/culture-nodes/internal/store/postgres"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "worker: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dbURL, err := requireEnv("WORKER_DB_URL")
	if err != nil {
		return err
	}
	workerID, err := requireEnv("WORKER_ID")
	if err != nil {
		return err
	}
	namespaceID, err := requireEnv("WORKER_NAMESPACE_ID")
	if err != nil {
		return err
	}
	leaseSeconds, err := requireEnvFloat("WORKER_LEASE_SECONDS")
	if err != nil {
		return err
	}
	limit, err := requireEnvInt("WORKER_LIMIT")
	if err != nil {
		return err
	}
	workMS, err := requireEnvInt("WORKER_WORK_MS")
	if err != nil {
		return err
	}
	idleTimeoutMS, err := requireEnvInt("WORKER_IDLE_TIMEOUT_MS")
	if err != nil {
		return err
	}
	flagFile := os.Getenv("WORKER_CLAIMED_FLAG_FILE")

	ctx := context.Background()
	s, err := postgres.Connect(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer s.Close()

	leaseDuration := time.Duration(leaseSeconds * float64(time.Second))
	workDuration := time.Duration(workMS) * time.Millisecond
	idleTimeout := time.Duration(idleTimeoutMS) * time.Millisecond

	lastProgress := time.Now()
	flagWritten := false

	for {
		if time.Since(lastProgress) > idleTimeout {
			fmt.Printf("worker %s: idle timeout after %s, exiting\n", workerID, idleTimeout)
			return nil
		}

		if _, err := s.ReclaimExpired(ctx); err != nil {
			return fmt.Errorf("ReclaimExpired: %w", err)
		}

		items, err := s.ClaimWork(ctx, namespaceID, workerID, leaseDuration, limit)
		if err != nil {
			return fmt.Errorf("ClaimWork: %w", err)
		}
		if len(items) == 0 {
			time.Sleep(75 * time.Millisecond)
			continue
		}
		lastProgress = time.Now()

		if flagFile != "" && !flagWritten {
			if werr := os.WriteFile(flagFile, []byte(strconv.Itoa(len(items))+"\n"), 0o644); werr != nil {
				return fmt.Errorf("write claimed flag file: %w", werr)
			}
			flagWritten = true
		}

		for _, item := range items {
			time.Sleep(workDuration)

			if _, err := s.Pool().Exec(ctx,
				`INSERT INTO test_work_results (work_id, node_run_id, attempt, completed_by) VALUES ($1, $2, $3, $4)`,
				item.ID, item.NodeRunID, item.Attempt, workerID,
			); err != nil && !isUniqueViolation(err) {
				return fmt.Errorf("insert result for %s: %w", item.ID, err)
			}
			// A unique violation here means this is either a re-delivery
			// of a work_id already recorded (should not happen -- work_id
			// is this claim's own primary key) or, for the duplicate
			// signal scenario, a second work item for the same
			// (node_run_id, attempt) whose sibling already recorded the
			// effective completion first. Either way, this worker still
			// performed and must still record its OWN technical
			// completion of the work item it was leased -- domain outcome
			// (the results row) and technical status (work_items.state)
			// are deliberately not the same thing.

			if err := s.CompleteWork(ctx, item.ID, workerID, item.FencingToken, int(item.Attempt)); err != nil {
				if errors.Is(err, postgres.ErrStaleClaim) {
					// Expected under contention: e.g. this worker was
					// reclaimed out from under itself. Not fatal.
					fmt.Printf("worker %s: stale claim completing %s (expected under contention)\n", workerID, item.ID)
					continue
				}
				return fmt.Errorf("CompleteWork %s: %w", item.ID, err)
			}
			fmt.Printf("worker %s: completed %s (node_run=%s attempt=%d fencing_token=%d)\n",
				workerID, item.ID, item.NodeRunID, item.Attempt, item.FencingToken)
		}
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
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

func requireEnvFloat(name string) (float64, error) {
	v, err := requireEnv(name)
	if err != nil {
		return 0, err
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	return f, nil
}
