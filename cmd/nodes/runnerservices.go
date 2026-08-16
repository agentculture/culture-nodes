package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
	clifmt.EmitResult(fmt.Sprintf("registered runner service %s in %s; restart each worker to load it", e.Name, path))
	return 0, nil
}

// runnerServiceConfig builds the worker's runner-service options from the
// environment. Every failure is an environment error with a remediation —
// a worker that half-loads its execution allowlist would dispatch some nodes
// and mysteriously refuse others.
func runnerServiceConfig() (worker.RunnerServiceOptions, *clifmt.CliError) {
	path := os.Getenv(envRunnerServicesFile)
	if path == "" {
		return worker.RunnerServiceOptions{}, nil
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- the operator names this file by env on purpose
	if err != nil {
		return worker.RunnerServiceOptions{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("reading %s %q: %v", envRunnerServicesFile, path, err),
			Remediation: "point NODES_RUNNER_SERVICES_FILE at a readable JSON file of runner services",
		}
	}
	var entries []runnerServiceEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		return worker.RunnerServiceOptions{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("parsing %q: %v", path, err),
			Remediation: `expected a JSON array: [{"name","endpoint","image_digest","secret_file",...}]`,
		}
	}
	if len(entries) == 0 {
		return worker.RunnerServiceOptions{}, nil
	}

	registry := runners.NewFunctionRegistry()
	secrets := runners.StaticSecrets{}
	for _, e := range entries {
		material, err := os.ReadFile(e.SecretFile) // #nosec G304 -- deployment-named secret file
		if err != nil {
			return worker.RunnerServiceOptions{}, &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("runner service %q: reading secret file %q: %v", e.Name, e.SecretFile, err),
				Remediation: "install the runner's bearer secret file (deploy/prod/install-secrets.sh) before starting the worker",
			}
		}
		// The registry wants a symbolic reference (never a path that could be
		// mistaken for material); the file path stays in this process only.
		ref := "runner-secret:" + e.Name
		secrets[ref] = strings.TrimSpace(string(material))
		identity := runners.ServiceIdentity{
			Endpoint:               e.Endpoint,
			ImageDigest:            e.ImageDigest,
			SecretRef:              ref,
			AllowInsecureTransport: e.AllowInsecureTransport,
			Description:            e.Description,
		}
		if err := registry.RegisterService(e.Name, identity); err != nil {
			return worker.RunnerServiceOptions{}, &clifmt.CliError{
				Code:        clifmt.ExitEnvError,
				Message:     fmt.Sprintf("runner service %q: %v", e.Name, err),
				Remediation: "fix the entry in " + path,
			}
		}
	}
	client, err := runners.NewProtocolClient(secrets)
	if err != nil {
		return worker.RunnerServiceOptions{}, &clifmt.CliError{
			Code:        clifmt.ExitEnvError,
			Message:     fmt.Sprintf("building the runner protocol client: %v", err),
			Remediation: "check the runner service entries in " + path,
		}
	}
	return worker.RunnerServiceOptions{Registry: registry, Client: client}, nil
}
