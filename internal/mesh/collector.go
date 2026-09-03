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
	TargetSource   func(context.Context) ([]Target, error)
}

type Target struct {
	Key    string
	URL    string
	Bearer string
	Error  string
}

type Observation struct {
	Hostname     string          `json:"hostname,omitempty"`
	Deployment   json.RawMessage `json:"deployment,omitempty"`
	ObservedAt   time.Time       `json:"observed_at"`
	Class        string          `json:"class,omitempty"`
	Reason       string          `json:"reason,omitempty"`
	Error        string          `json:"error,omitempty"`
	FailureCount uint64          `json:"failure_count,omitempty"`
}

type Collector struct {
	config   Config
	mu       sync.RWMutex
	targets  []Target
	cache    map[string]Observation
	failures map[string]uint64
	reported map[string]string
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
	return &Collector{config: config, cache: make(map[string]Observation), failures: make(map[string]uint64), reported: make(map[string]string)}
}

func (c *Collector) SetTargets(targets []Target) {
	c.mu.Lock()
	c.targets = append([]Target(nil), targets...)
	current := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		current[target.Key] = struct{}{}
	}
	for key := range c.cache {
		if _, ok := current[key]; !ok {
			delete(c.cache, key)
			delete(c.failures, key)
			delete(c.reported, key)
		}
	}
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
	if c.config.TargetSource != nil {
		targets, err := c.config.TargetSource(ctx)
		if err != nil {
			c.config.Logger.Warn("mesh target refresh failed", "error", err)
		} else {
			c.SetTargets(targets)
		}
	}
	c.mu.RLock()
	targets := append([]Target(nil), c.targets...)
	c.mu.RUnlock()
	sem := make(chan struct{}, c.config.MaxConcurrency)
	var wg sync.WaitGroup
	for _, target := range targets {
		target := target
		if target.Error != "" {
			c.storeNonFailure(target.Key, "unobserved", target.Error)
			continue
		}
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
	url := target.URL
	if !strings.HasSuffix(strings.TrimRight(url, "/"), actors.CapabilitiesPath) {
		url = strings.TrimRight(url, "/") + actors.CapabilitiesPath
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err == nil && target.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+target.Bearer)
	}
	if err == nil {
		var response *http.Response
		response, err = c.config.HTTPClient.Do(req)
		if err == nil {
			defer response.Body.Close()
			if response.StatusCode >= http.StatusBadRequest && response.StatusCode < http.StatusInternalServerError {
				c.storeNonFailure(target.Key, "unsupported", fmt.Sprintf("GET capabilities: %s", response.Status))
				return
			} else if response.StatusCode != http.StatusOK {
				err = fmt.Errorf("GET capabilities: %s", response.Status)
			} else {
				body, readErr := io.ReadAll(io.LimitReader(response.Body, maxProbeResponseBytes+1))
				if readErr != nil {
					err = readErr
				} else if len(body) > maxProbeResponseBytes {
					c.storeNonFailure(target.Key, "unsupported", fmt.Sprintf("capabilities response exceeds %d bytes", maxProbeResponseBytes))
					return
				} else {
					var payload struct {
						Preflight struct {
							Host struct {
								Hostname   string          `json:"hostname"`
								Deployment json.RawMessage `json:"deployment"`
							} `json:"host"`
						} `json:"preflight"`
					}
					if decodeErr := json.Unmarshal(body, &payload); decodeErr != nil {
						c.storeNonFailure(target.Key, "unsupported", fmt.Sprintf("decode capabilities: %v", decodeErr))
						return
					} else if payload.Preflight.Host.Hostname == "" {
						c.storeNonFailure(target.Key, "unsupported", "capabilities has no preflight.host.hostname")
						return
					} else if len(payload.Preflight.Host.Deployment) == 0 {
						c.storeNonFailure(target.Key, "unsupported", "capabilities has no preflight.host.deployment block")
						return
					} else {
						c.store(target.Key, Observation{Hostname: payload.Preflight.Host.Hostname, Deployment: payload.Preflight.Host.Deployment, ObservedAt: observedAt})
						return
					}
				}
			}
		}
	}
	c.mu.Lock()
	c.failures[target.Key]++
	count := c.failures[target.Key]
	c.cache[target.Key] = Observation{ObservedAt: observedAt, Class: "failed", Error: err.Error(), FailureCount: count}
	delete(c.reported, target.Key)
	c.mu.Unlock()
	c.config.Logger.Warn("mesh bridge probe", "target", target.Key, "class", "failed", "failure_count", count, "error", err)
}

func (c *Collector) storeNonFailure(key, class, errText string) {
	observedAt := time.Now().UTC()
	c.mu.Lock()
	observation := Observation{ObservedAt: observedAt, Class: class, Error: errText}
	if class == "unsupported" {
		observation.Reason = errText
	}
	c.cache[key] = observation
	alreadyReported := c.reported[key] == class+"\x00"+errText
	c.reported[key] = class + "\x00" + errText
	c.mu.Unlock()
	if !alreadyReported {
		c.config.Logger.Info("mesh bridge probe", "target", key, "class", class, "error", errText)
	}
}

func (c *Collector) store(key string, observation Observation) {
	c.mu.Lock()
	c.cache[key] = observation
	delete(c.reported, key)
	c.mu.Unlock()
}
