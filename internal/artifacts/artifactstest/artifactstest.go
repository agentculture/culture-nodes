// Package artifactstest provides ephemeral-Docker test fixtures shared by
// internal/artifacts and its postgres/s3 driver subpackages: a throwaway
// PostgreSQL instance (the metadata authority for every artifact regardless
// of backend) and a throwaway MinIO instance (S3-compatible object
// storage). It mirrors the bootstrap pattern in
// internal/store/postgres/testmain_test.go -- Docker picks the host port,
// the caller waits for health, and the returned stop func removes the
// container -- adapted here into a reusable package so three different
// test packages (internal/artifacts, internal/artifacts/postgres,
// internal/artifacts/s3) do not each reimplement it.
//
// This is a regular (non-_test.go) package, the same way the standard
// library's net/http/httptest is: its functions are meant to be called only
// from tests, but Go cannot import a package's _test.go files from another
// package, so the fixtures have to live in ordinary .go files instead.
package artifactstest

import (
	"context"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// StartPostgres starts postgres:17-alpine detached with Docker (choosing
// the host port, to avoid collisions with anything already listening),
// waits for it to accept connections, and returns its connection URL and a
// stop function. It returns a non-nil error -- without touching any
// *testing.T -- when Docker is not usable at all, so the caller decides
// whether that means skip or fail.
func StartPostgres(ctx context.Context) (dbURL string, stop func(), err error) {
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		return "", nil, fmt.Errorf("docker not found on PATH: %w", lookErr)
	}

	name := containerName("nodes-artifacts-pg")

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	runCmd := exec.CommandContext(runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "POSTGRES_PASSWORD=nodes",
		"-e", "POSTGRES_DB=nodes",
		"-p", "5432",
		"postgres:17-alpine",
	)
	if out, runErr := runCmd.CombinedOutput(); runErr != nil {
		return "", nil, fmt.Errorf("docker run postgres:17-alpine: %w (%s)", runErr, strings.TrimSpace(string(out)))
	}
	stopFn := stopper(name)

	port, portErr := hostPort(ctx, name, "5432/tcp")
	if portErr != nil {
		stopFn()
		return "", nil, fmt.Errorf("docker port %s: %w", name, portErr)
	}

	url := fmt.Sprintf("postgres://postgres:nodes@127.0.0.1:%s/nodes?sslmode=disable", port)

	if waitErr := waitForPostgres(ctx, url, 45*time.Second); waitErr != nil {
		stopFn()
		return "", nil, fmt.Errorf("postgres container %s did not become ready: %w", name, waitErr)
	}

	return url, stopFn, nil
}

// StartMinIO starts minio/minio detached with Docker in single-node mode,
// waits for its S3 API to report healthy, and returns its endpoint
// (host:port, no scheme), a fixed access/secret key pair the container was
// given at startup (these are not real secrets -- there is nothing durable
// behind an ephemeral, --rm'd container), and a stop function. It returns a
// non-nil error -- without touching any *testing.T -- when Docker is not
// usable at all, so the caller decides whether that means skip or fail.
func StartMinIO(ctx context.Context) (endpoint, accessKey, secretKey string, stop func(), err error) {
	if _, lookErr := exec.LookPath("docker"); lookErr != nil {
		return "", "", "", nil, fmt.Errorf("docker not found on PATH: %w", lookErr)
	}

	const (
		rootUser     = "nodesartifacts"
		rootPassword = "nodesartifacts123"
	)

	name := containerName("nodes-artifacts-minio")

	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	runCmd := exec.CommandContext(runCtx, "docker", "run", "-d", "--rm",
		"--name", name,
		"-e", "MINIO_ROOT_USER="+rootUser,
		"-e", "MINIO_ROOT_PASSWORD="+rootPassword,
		"-p", ":9000",
		"minio/minio", "server", "/data",
	)
	if out, runErr := runCmd.CombinedOutput(); runErr != nil {
		return "", "", "", nil, fmt.Errorf("docker run minio/minio: %w (%s)", runErr, strings.TrimSpace(string(out)))
	}
	stopFn := stopper(name)

	port, portErr := hostPort(ctx, name, "9000/tcp")
	if portErr != nil {
		stopFn()
		return "", "", "", nil, fmt.Errorf("docker port %s: %w", name, portErr)
	}
	endpoint = fmt.Sprintf("127.0.0.1:%s", port)

	if waitErr := waitForMinIO(ctx, endpoint, 45*time.Second); waitErr != nil {
		stopFn()
		return "", "", "", nil, fmt.Errorf("minio container %s did not become ready: %w", name, waitErr)
	}

	return endpoint, rootUser, rootPassword, stopFn, nil
}

func containerName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func stopper(name string) func() {
	return func() {
		stopCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = exec.CommandContext(stopCtx, "docker", "stop", name).Run()
	}
}

// hostPort returns the host port Docker mapped to containerPort (e.g.
// "5432/tcp") on the named container.
func hostPort(ctx context.Context, name, containerPort string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "port", name, containerPort).Output()
	if err != nil {
		return "", err
	}
	// Output looks like "0.0.0.0:54321\n[::]:54321\n"; take the port after
	// the last colon on the first line.
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	idx := strings.LastIndex(line, ":")
	if idx == -1 || idx == len(line)-1 {
		return "", fmt.Errorf("unexpected `docker port` output: %q", string(out))
	}
	return line[idx+1:], nil
}

func waitForPostgres(ctx context.Context, url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		lastErr = pingPostgresOnce(ctx, url)
		if lastErr == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return lastErr
}

func pingPostgresOnce(ctx context.Context, url string) error {
	connectCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()

	pool, err := pgxpool.New(connectCtx, url)
	if err != nil {
		return err
	}
	defer pool.Close()

	return pool.Ping(connectCtx)
}

func waitForMinIO(ctx context.Context, endpoint string, timeout time.Duration) error {
	healthURL := fmt.Sprintf("http://%s/minio/health/live", endpoint)
	client := &http.Client{Timeout: 2 * time.Second}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
		if err != nil {
			return fmt.Errorf("build health request: %w", err)
		}
		resp, reqErr := client.Do(req)
		if reqErr == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("health check: status %d", resp.StatusCode)
		} else {
			lastErr = reqErr
		}
		time.Sleep(300 * time.Millisecond)
	}
	return lastErr
}
