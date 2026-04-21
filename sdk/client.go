// Package sdk provides a Go client for PaladinCore.
//
// Day6: Three-phase lifecycle from Paladin SDK V2.
// 1. Startup: full pull (or fallback to local cache)
// 2. Runtime: long-poll for incremental updates
// 3. Shutdown: graceful stop
package sdk

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"paladin-core/internal/logger"
)

// Config holds SDK configuration.
type Config struct {
	Addrs        []string
	Tenant       string
	Namespace    string
	CacheDir     string        // Local cache for fallback
	PollTimeout  time.Duration // Default 30s
	RetryBackoff time.Duration // Default 1s
}

// Client is the PaldinCore SDK client.
type Client struct {
	config   Config
	mu       sync.RWMutex
	configs  map[string][]byte
	revision uint64
	watchers map[string][]func(string, []byte, []byte)
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	client   *http.Client
	log      *slog.Logger
	metrics  *clientMetrics
}

type configResponse struct {
	Revision uint64       `json:"revision"`
	Configs  []configItem `json:"configs"`
}

type configItem struct {
	Key      string `json:"key"`
	Value    string `json:"value"`
	Revision uint64 `json:"revision"`
}

type watchResponse struct {
	Revision uint64       `json:"revision"`
	Events   []watchEvent `json:"events"`
}

type watchEvent struct {
	Type      string      `json:"type"`
	Entry     *configItem `json:"entry"`
	PrevEntry *configItem `json:"prev_entry,omitempty"`
}

// New creates a client, does full pull, starts watch loop.
func New(cfg Config) (*Client, error) {
	if cfg.PollTimeout == 0 {
		cfg.PollTimeout = 30 * time.Second
	}
	if cfg.RetryBackoff == 0 {
		cfg.RetryBackoff = 1 * time.Second
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{
		config:   cfg,
		configs:  make(map[string][]byte),
		watchers: make(map[string][]func(string, []byte, []byte)),
		ctx:      ctx,
		cancel:   cancel,
		client:   &http.Client{Timeout: cfg.PollTimeout + 5*time.Second},
		log:      logger.L("sdk").With("tenant", cfg.Tenant, "namespace", cfg.Namespace),
		metrics:  newClientMetrics(cfg.Tenant, cfg.Namespace),
	}
	if err := c.fullPull(); err != nil {
		c.log.Warn("full pull failed, falling back to cache", "err", err)
		if cacheErr := c.loadFromCache(); cacheErr != nil {
			c.log.Error("cache load failed", "err", cacheErr)
		}
	}
	c.wg.Add(1)
	go c.watchLoop()
	return c, nil
}

func (c *Client) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.configs[key]
	return v, ok
}

func (c *Client) GetAll() map[string][]byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m := make(map[string][]byte, len(c.configs))
	for k, v := range c.configs {
		m[k] = v
	}
	return m
}

// OnChange registers a callback. key="" watches all keys.
func (c *Client) OnChange(key string, fn func(string, []byte, []byte)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.watchers[key] = append(c.watchers[key], fn)
}

func (c *Client) Close() {
	c.cancel()
	c.wg.Wait()
}

// MetricsRegistry returns the Prometheus registry scoped to this Client.
// Expose it alongside your app's own metrics, e.g.:
//
//	http.Handle("/sdk-metrics",
//	    promhttp.HandlerFor(c.MetricsRegistry(), promhttp.HandlerOpts{}))
//
// Or merge with other registries via prometheus.Gatherers. See
// `sdk/metrics.go` for the list of exposed series.
func (c *Client) MetricsRegistry() *prometheus.Registry {
	return c.metrics.registry
}

func (c *Client) fullPull() (err error) {
	start := time.Now()
	// Named return + deferred metrics block keeps the instrumentation out
	// of the existing error-return sites below.
	defer func() {
		c.metrics.fullPullDuration.Observe(time.Since(start).Seconds())
		outcome := "ok"
		if err != nil {
			outcome = "error"
		}
		c.metrics.fullPullsTotal.WithLabelValues(outcome).Inc()
	}()

	url := fmt.Sprintf("http://%s/api/v1/config/%s/%s",
		c.pickAddr(),
		c.config.Tenant,
		c.config.Namespace,
	)
	resp, err := c.client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("status %d: %s", resp.StatusCode, string(body))
	}
	var cr configResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return err
	}
	c.mu.Lock()
	for _, item := range cr.Configs {
		c.configs[item.Key] = []byte(item.Value)
	}
	c.revision = cr.Revision
	count := len(c.configs)
	c.mu.Unlock()
	c.metrics.revision.Set(float64(cr.Revision))
	c.metrics.configs.Set(float64(count))
	c.saveToCache()
	c.log.Info("full pull complete", "count", len(cr.Configs), "rev", cr.Revision)
	return nil
}

func (c *Client) watchLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		default:
		}
		c.mu.RLock()
		rev := c.revision
		c.mu.RUnlock()
		url := fmt.Sprintf("http://%s/api/v1/watch/%s/%s/?revision=%d&timeout=%d",
			c.pickAddr(), c.config.Tenant, c.config.Namespace,
			rev, int(c.config.PollTimeout.Seconds()),
		)
		start := time.Now()
		resp, err := c.client.Get(url)
		if err != nil {
			c.metrics.watchPollDuration.Observe(time.Since(start).Seconds())
			c.metrics.watchPollsTotal.WithLabelValues("error").Inc()
			c.log.Warn("watch request failed, retrying", "err", err, "backoff", c.config.RetryBackoff)
			c.sleep(c.config.RetryBackoff)
			continue
		}
		var wr watchResponse
		if err := json.NewDecoder(resp.Body).Decode(&wr); err != nil {
			c.metrics.watchPollDuration.Observe(time.Since(start).Seconds())
			c.metrics.watchPollsTotal.WithLabelValues("error").Inc()
			c.log.Error("decode watch response", "err", err)
			resp.Body.Close()
			continue
		}
		resp.Body.Close()
		c.metrics.watchPollDuration.Observe(time.Since(start).Seconds())
		outcome := "empty"
		if len(wr.Events) > 0 {
			outcome = "events"
		}
		c.metrics.watchPollsTotal.WithLabelValues(outcome).Inc()

		if len(wr.Events) > 0 {
			c.applyEvents(wr.Events)
			c.saveToCache()
			c.mu.RLock()
			count := len(c.configs)
			c.mu.RUnlock()
			c.metrics.configs.Set(float64(count))
		}
		if wr.Revision > 0 {
			c.mu.Lock()
			c.revision = wr.Revision
			c.mu.Unlock()
			c.metrics.revision.Set(float64(wr.Revision))
		}
	}
}

func (c *Client) applyEvents(events []watchEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range events {
		key := e.Entry.Key
		newVal := []byte(e.Entry.Value)
		oldVal := c.configs[key]
		switch e.Type {
		case "PUT":
			c.configs[key] = newVal
		case "DELETE":
			delete(c.configs, key)
			newVal = nil
		}
		c.metrics.watchEventsTotal.WithLabelValues(e.Type).Inc()
		for _, fn := range c.watchers[key] {
			fn(key, oldVal, newVal)
		}
		for _, fn := range c.watchers[""] {
			fn(key, oldVal, newVal)
		}
		c.log.Debug("event applied", "type", e.Type, "key", key)
	}
}

func (c *Client) pickAddr() string {
	return c.config.Addrs[0]
}

func (c *Client) sleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-c.ctx.Done():
	}
}

// --- Local Cache with SHA-256 Checksum ---
type cacheFile struct {
	CheckSum string            `json:"checksum"`
	Revision uint64            `json:"revision"`
	Configs  map[string]string `json:"configs"`
}

func (c *Client) cachePath() string {
	if c.config.CacheDir == "" {
		return ""
	}
	return filepath.Join(c.config.CacheDir, fmt.Sprintf("paladin_%s_%s.json", c.config.Tenant, c.config.Namespace))
}

func (c *Client) saveToCache() {
	path := c.cachePath()
	if path == "" {
		return
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	c.mu.RLock()
	cfgs := make(map[string]string, len(c.configs))
	for k, v := range c.configs {
		cfgs[k] = string(v)
	}
	c.mu.RUnlock()

	data, _ := json.Marshal(cfgs)
	cf := cacheFile{CheckSum: sha256Sum(data), Revision: c.revision, Configs: cfgs}
	out, _ := json.MarshalIndent(cf, "", "  ")
	os.WriteFile(path, out, 0644)
}

func (c *Client) loadFromCache() error {
	path := c.cachePath()
	if path == "" {
		c.metrics.cacheLoadsTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("no cache dir")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		c.metrics.cacheLoadsTotal.WithLabelValues("error").Inc()
		return err
	}
	var cf cacheFile
	if err := json.Unmarshal(data, &cf); err != nil {
		c.metrics.cacheLoadsTotal.WithLabelValues("error").Inc()
		return fmt.Errorf("corrupt cache: %w", err)
	}
	cfgData, _ := json.Marshal(cf.Configs)
	if sha256Sum(cfgData) != cf.CheckSum {
		c.metrics.cacheLoadsTotal.WithLabelValues("checksum_mismatch").Inc()
		return fmt.Errorf("cache checksum mismatch")
	}
	c.mu.Lock()
	for k, v := range cf.Configs {
		c.configs[k] = []byte(v)
	}
	c.revision = cf.Revision
	count := len(c.configs)
	c.mu.Unlock()
	c.metrics.cacheLoadsTotal.WithLabelValues("ok").Inc()
	c.metrics.revision.Set(float64(cf.Revision))
	c.metrics.configs.Set(float64(count))
	c.log.Info("loaded from cache", "count", len(cf.Configs), "rev", cf.Revision)
	return nil
}

func sha256Sum(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
