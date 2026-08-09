package main

import (
	"encoding/json"
	"fmt"
	"os"
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
