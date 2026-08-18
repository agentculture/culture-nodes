package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/agentculture/culture-nodes/internal/clifmt"
	"github.com/agentculture/culture-nodes/internal/runners"
	"github.com/agentculture/culture-nodes/internal/worker"
)

// envRunnerServicesFile points at the deployment's runner-service registry: a
// JSON array of entries binding a registry name (a NodeKey or a node's `uses`
// reference) to a runner service. Absent or empty, the worker keeps the
// in-process CodeRunner path only — dispatching code over the network is a
// deployment decision, never a default.
const envRunnerServicesFile = "NODES_RUNNER_SERVICES_FILE"

// runnerServiceEntry is one configured runner service. secret_file holds the
// bearer material outside this file so the registry JSON stays loggable and
// committable; the path itself is the registry's secret *reference*.
type runnerServiceEntry struct {
	Name                   string `json:"name"`
	Endpoint               string `json:"endpoint"`
	ImageDigest            string `json:"image_digest"`
	SecretFile             string `json:"secret_file"`
	AllowInsecureTransport bool   `json:"allow_insecure_transport"`
	Description            string `json:"description"`
}

func loadRunnerServiceEntries(path string) ([]runnerServiceEntry, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- operator-selected registry
	if err != nil {
		return nil, err
	}
	var entries []runnerServiceEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

func validateRunnerServiceEntry(e runnerServiceEntry) error {
	r := runners.NewFunctionRegistry()
	return r.RegisterService(e.Name, runners.ServiceIdentity{
		Endpoint: e.Endpoint, ImageDigest: e.ImageDigest, SecretRef: "runner-secret:" + e.Name,
		AllowInsecureTransport: e.AllowInsecureTransport, Description: e.Description,
	})
}

func registerRunnerServiceFile(path string, entry runnerServiceEntry) error {
	if err := validateRunnerServiceEntry(entry); err != nil {
		return err
	}
	entries := []runnerServiceEntry{}
	if _, err := os.Stat(path); err == nil {
		var loadErr error
		entries, loadErr = loadRunnerServiceEntries(path)
		if loadErr != nil {
			return loadErr
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, existing := range entries {
		if existing.Name == entry.Name {
			return fmt.Errorf("runner service %q is already registered", entry.Name)
		}
	}
	entries = append(entries, entry)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	raw, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".runner-services-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

func cmdRunnerServices(args []string, jsonMode bool) (int, error) {
	if len(args) == 0 || (args[0] != "list" && args[0] != "register") {
		return 0, &clifmt.CliError{Code: clifmt.ExitUserError, Message: "expected runner-services list or runner-services register", Remediation: "set " + envRunnerServicesFile + " and choose list or register"}
	}
	path := strings.TrimSpace(os.Getenv(envRunnerServicesFile))
	if path == "" {
		return 0, &clifmt.CliError{Code: clifmt.ExitEnvError, Message: envRunnerServicesFile + " is not set", Remediation: "point it at the worker's runner-service JSON file"}
	}
	if args[0] == "list" {
		if len(args) != 1 {
			return 0, parseError("runner-services list", fmt.Errorf("unexpected arguments"))
		}
		entries, err := loadRunnerServiceEntries(path)
		if err != nil {
			return 0, &clifmt.CliError{Code: clifmt.ExitEnvError, Message: fmt.Sprintf("reading %s: %v", path, err), Remediation: "verify the registry file is readable JSON"}
		}
		if jsonMode {
			return 0, clifmt.EmitResultJSON(entries)
		}
		if len(entries) == 0 {
			clifmt.EmitResult("no runner services registered")
			return 0, nil
		}
		var lines []string
		for _, e := range entries {
			lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s", e.Name, e.Endpoint, e.ImageDigest, e.SecretFile))
		}
		clifmt.EmitResult(strings.Join(lines, "\n"))
		return 0, nil
	}
	fs := newFlagSet("runner-services register")
	e := runnerServiceEntry{}
	fs.StringVar(&e.Name, "name", "", "registry name")
	fs.StringVar(&e.Endpoint, "endpoint", "", "runner protocol base URL")
	fs.StringVar(&e.ImageDigest, "image-digest", "", "pinned sha256 digest")
	fs.StringVar(&e.SecretFile, "secret-file", "", "bearer secret file")
	fs.BoolVar(&e.AllowInsecureTransport, "allow-insecure-transport", false, "permit plaintext non-loopback HTTP")
	fs.StringVar(&e.Description, "description", "", "operator-facing description")
	if err := fs.Parse(args[1:]); err != nil {
		return 0, parseError("runner-services register", err)
	}
	if e.SecretFile == "" {
		return 0, &clifmt.CliError{Code: clifmt.ExitUserError, Message: "--secret-file is required", Remediation: "name the installed bearer secret file"}
	}
	if err := registerRunnerServiceFile(path, e); err != nil {
		return 0, &clifmt.CliError{Code: clifmt.ExitEnvError, Message: fmt.Sprintf("registering runner service: %v", err), Remediation: "fix the registration fields or registry file"}
	}
	if jsonMode {
		return 0, clifmt.EmitResultJSON(e)
	}
	clifmt.EmitResult(fmt.Sprintf("registered runner service %s in %s; a running worker picks it up on its next reload check, no restart required", e.Name, path))
	return 0, nil
}

// buildRunnerServices turns parsed registry entries into the two things a
// registration needs: the identities to register (keyed by name) and the
// bearer material each one's SecretRef resolves to. It is the shared core of
// both the worker's initial load and a later reload — the two must build the
// exact same shape from the exact same file, or "reload" would mean
// something subtly different from "start".
func buildRunnerServices(entries []runnerServiceEntry) (map[string]runners.ServiceIdentity, runners.StaticSecrets, *clifmt.CliError) {
	services := make(map[string]runners.ServiceIdentity, len(entries))
	secrets := runners.StaticSecrets{}
	for _, e := range entries {
		material, err := os.ReadFile(e.SecretFile) // #nosec G304 -- deployment-named secret file
		if err != nil {
			return nil, nil, &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("runner service %q: reading secret file %q: %v", e.Name, e.SecretFile, err),
				Remediation: "install the runner's bearer secret file (deploy/prod/install-secrets.sh) before starting the worker",
			}
		}
		// The registry wants a symbolic reference (never a path that could be
		// mistaken for material); the file path stays in this process only.
		ref := "runner-secret:" + e.Name
		secrets[ref] = strings.TrimSpace(string(material))
		services[e.Name] = runners.ServiceIdentity{
			Endpoint:               e.Endpoint,
			ImageDigest:            e.ImageDigest,
			SecretRef:              ref,
			AllowInsecureTransport: e.AllowInsecureTransport,
			Description:            e.Description,
		}
	}
	return services, secrets, nil
}

// runnerServiceReloader re-reads NODES_RUNNER_SERVICES_FILE and applies a
// changed registry to an already-running worker's registry and secrets,
// without a process restart (task t19, issue #8's "runner services load at
// worker start only" residue).
//
// It holds the same *runners.FunctionRegistry and *runners.ReloadableSecrets
// the worker was built with — not a copy of them — so a successful reload is
// visible to the worker's very next dispatch: nothing in the worker itself
// has to be told a reload happened. It is deliberately mtime-gated: a check
// that finds the file unchanged does no parsing, no secret-file reads, and
// touches neither the registry nor the secrets — an operator who has changed
// nothing pays nothing beyond one stat(2) per check.
type runnerServiceReloader struct {
	path     string
	registry *runners.FunctionRegistry
	secrets  *runners.ReloadableSecrets
	lastMod  time.Time
}

// checkAndReload reloads the registry only if the file's mtime has advanced
// since the last successful load (construction counts as one). It reports
// whether a reload was applied, so a caller — this file's tests, or poll
// below — can tell "unchanged" from "changed and applied" without inspecting
// the registry itself.
//
// A failed reload changes nothing: the registry and secrets this worker
// already validated keep dispatching exactly as before, the same way a
// malformed file at startup refuses to start rather than half-apply. The
// broken file is reported, not silently retried into the void — the next
// tick will try again and keep trying until the operator fixes it.
func (rl *runnerServiceReloader) checkAndReload() (bool, *clifmt.CliError) {
	info, err := os.Stat(rl.path)
	if err != nil {
		return false, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("stat %s: %v", rl.path, err),
			Remediation: "verify " + envRunnerServicesFile + " still names a readable file",
		}
	}
	if !info.ModTime().After(rl.lastMod) {
		return false, nil
	}
	entries, err := loadRunnerServiceEntries(rl.path)
	if err != nil {
		return false, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("reloading %s: %v", rl.path, err),
			Remediation: "verify the registry file is readable JSON",
		}
	}
	services, secrets, cliErr := buildRunnerServices(entries)
	if cliErr != nil {
		return false, cliErr
	}
	// The registry swap and the secrets swap are two separate atomic writes,
	// not one transaction, so the ORDER decides what the brief window
	// between them can look like. Secrets go FIRST: a superset secret map
	// beside the old registry is harmless (an unresolvable name never asks
	// for its credential), whereas the reverse order lets a dispatch resolve
	// a newly added identity whose SecretRef is not yet in the map and fail
	// authentication for no reason a caller can see (PR #180 review
	// finding — the original ordering argued the opposite and was wrong).
	rl.secrets.Store(secrets)
	if err := rl.registry.ReloadServices(services); err != nil {
		return false, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("reloading runner services: %v", err),
			Remediation: "fix the entries in " + rl.path,
		}
	}
	rl.lastMod = info.ModTime()
	return true, nil
}

// poll runs checkAndReload every interval until ctx is done, reporting a
// failed reload through onError. It never stops on a failure: a config file
// left broken for one interval must not cost every interval after it too.
func (rl *runnerServiceReloader) poll(ctx context.Context, interval time.Duration, onError func(error)) {
	if interval <= 0 {
		interval = worker.DefaultPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, cliErr := rl.checkAndReload(); cliErr != nil && onError != nil {
				onError(cliErr)
			}
		}
	}
}

// runnerServiceConfig builds the worker's runner-service options from the
// environment, and — when the protocol path is enabled — a reloader that can
// apply a later change to the same file without rebuilding either the
// registry or the client. Every failure is an environment error with a
// remediation — a worker that half-loads its execution allowlist would
// dispatch some nodes and mysteriously refuse others.
//
// The returned reloader is nil exactly when the protocol path is disabled
// (no env, or an empty registry file): a worker with nothing to dispatch to
// runner services has nothing worth watching for changes either.
func runnerServiceConfig() (worker.RunnerServiceOptions, *runnerServiceReloader, *clifmt.CliError) {
	path := strings.TrimSpace(os.Getenv(envRunnerServicesFile))
	if path == "" {
		return worker.RunnerServiceOptions{}, nil, nil
	}
	info, err := os.Stat(path)
	if err != nil {
		return worker.RunnerServiceOptions{}, nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("reading %s %q: %v", envRunnerServicesFile, path, err),
			Remediation: "point NODES_RUNNER_SERVICES_FILE at a readable JSON file of runner services",
		}
	}
	entries, err := loadRunnerServiceEntries(path)
	if err != nil {
		return worker.RunnerServiceOptions{}, nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("parsing %q: %v", path, err),
			Remediation: `expected a JSON array: [{"name","endpoint","image_digest","secret_file",...}]`,
		}
	}
	// A CONFIGURED registry that is currently empty still builds the full
	// reloadable plumbing: a worker started before its first registration
	// must observe later additions without a restart, which is the whole
	// point of the reloader (PR #180 review finding). Only an UNSET env var
	// disables the protocol path, and that decision happened above.
	services, secretsMap, cliErr := buildRunnerServices(entries)
	if cliErr != nil {
		return worker.RunnerServiceOptions{}, nil, cliErr
	}

	registry := runners.NewFunctionRegistry()
	if err := registry.ReloadServices(services); err != nil {
		return worker.RunnerServiceOptions{}, nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("registering runner services: %v", err),
			Remediation: "fix the entries in " + path,
		}
	}
	secrets := runners.NewReloadableSecrets(secretsMap)
	client, err := runners.NewProtocolClient(secrets)
	if err != nil {
		return worker.RunnerServiceOptions{}, nil, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the runner protocol client: %v", err),
			Remediation: "check the runner service entries in " + path,
		}
	}
	reloader := &runnerServiceReloader{path: path, registry: registry, secrets: secrets, lastMod: info.ModTime()}
	return worker.RunnerServiceOptions{Registry: registry, Client: client}, reloader, nil
}
