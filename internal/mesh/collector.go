// Package mesh maintains the out-of-band bridge observations rendered by
// the mesh read model. HTTP handlers only read its cache; they never probe.
package mesh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentculture/culture-nodes/internal/actors"
)

const maxProbeResponseBytes = 1 << 20

type Config struct {
	Interval       time.Duration
	ProbeTimeout   time.Duration
	MaxConcurrency int
	HTTPClient     *http.Client
	Logger         *slog.Logger
}

type Target struct {
	Key string
	URL string
}

type Observation struct {
	Deployment json.RawMessage `json:"deployment,omitempty"`
	ObservedAt time.Time       `json:"observed_at"`
	Error      string          `json:"error,omitempty"`
}

type Collector struct {
	config   Config
	mu       sync.RWMutex
	targets  []Target
	cache    map[string]Observation
	failures map[string]uint64
}

func New(config Config) *Collector {
	if config.Interval <= 0 {
		config.Interval = 30 * time.Second
	}
	if config.ProbeTimeout <= 0 {
		config.ProbeTimeout = 2 * time.Second
	}
	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = 4
	}
	if config.HTTPClient == nil {
		config.HTTPClient = http.DefaultClient
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	return &Collector{config: config, cache: make(map[string]Observation), failures: make(map[string]uint64)}
}

func (c *Collector) SetTargets(targets []Target) {
	c.mu.Lock()
	c.targets = append([]Target(nil), targets...)
	c.mu.Unlock()
}

func (c *Collector) Snapshot() map[string]Observation {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make(map[string]Observation, len(c.cache))
	for key, value := range c.cache {
		value.Deployment = append(json.RawMessage(nil), value.Deployment...)
		out[key] = value
	}
	return out
}

func (c *Collector) FailureCount(key string) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.failures[key]
}

func (c *Collector) Run(ctx context.Context) {
	c.Collect(ctx)
	ticker := time.NewTicker(c.config.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Collect(ctx)
		}
	}
}

func (c *Collector) Collect(ctx context.Context) {
	c.mu.RLock()
	targets := append([]Target(nil), c.targets...)
	c.mu.RUnlock()
	sem := make(chan struct{}, c.config.MaxConcurrency)
	var wg sync.WaitGroup
	for _, target := range targets {
		target := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			c.probe(ctx, target)
		}()
	}
	wg.Wait()
}

func (c *Collector) probe(parent context.Context, target Target) {
	observedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(parent, c.config.ProbeTimeout)
	defer cancel()
	url := strings.TrimRight(target.URL, "/") + actors.CapabilitiesPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err == nil {
		var response *http.Response
		response, err = c.config.HTTPClient.Do(req)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("GET capabilities: %s", response.Status)
			} else {
				body, readErr := io.ReadAll(io.LimitReader(response.Body, maxProbeResponseBytes+1))
				if readErr != nil {
					err = readErr
				} else if len(body) > maxProbeResponseBytes {
					err = fmt.Errorf("capabilities response exceeds %d bytes", maxProbeResponseBytes)
				} else {
					var payload struct {
						Preflight struct {
							Host struct {
								Deployment json.RawMessage `json:"deployment"`
							} `json:"host"`
						} `json:"preflight"`
					}
					if decodeErr := json.Unmarshal(body, &payload); decodeErr != nil {
						err = fmt.Errorf("decode capabilities: %w", decodeErr)
					} else if len(payload.Preflight.Host.Deployment) == 0 {
						err = fmt.Errorf("capabilities has no preflight.host.deployment block")
					} else {
						c.store(target.Key, Observation{Deployment: payload.Preflight.Host.Deployment, ObservedAt: observedAt})
						return
					}
				}
			}
		}
	}
	c.mu.Lock()
	c.failures[target.Key]++
	count := c.failures[target.Key]
	c.cache[target.Key] = Observation{ObservedAt: observedAt, Error: err.Error()}
	c.mu.Unlock()
	c.config.Logger.Warn("mesh bridge probe failed", "target", target.Key, "failure_count", count, "error", err)
}

func (c *Collector) store(key string, observation Observation) {
	c.mu.Lock()
	c.cache[key] = observation
	c.mu.Unlock()
}
