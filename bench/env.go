package bench

import (
	"os"
	"runtime"
	"time"
)

// Env is the environment fingerprint attached to every Result.
// Benchmarks are only comparable when their Env matches, so we record
// everything that plausibly affects performance: Go version, CPU count,
// GOMAXPROCS, OS/arch, and hostname. PaladinBuild is optional and, if set
// (via `PALADIN_BUILD` env var at launch), pins the result to a specific
// git SHA or tag for regression tracking.
type Env struct {
	Timestamp    time.Time `json:"timestamp"`
	Go           string    `json:"go_version"`
	GOOS         string    `json:"goos"`
	GOARCH       string    `json:"goarch"`
	NumCPU       int       `json:"num_cpu"`
	GOMAXPROCS   int       `json:"gomaxprocs"`
	Hostname     string    `json:"hostname"`
	PaladinBuild string    `json:"paladin_build,omitempty"`
}

// CollectEnv snapshots the current runtime environment.
func CollectEnv() Env {
	host, _ := os.Hostname()
	return Env{
		Timestamp:    time.Now(),
		Go:           runtime.Version(),
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
		NumCPU:       runtime.NumCPU(),
		GOMAXPROCS:   runtime.GOMAXPROCS(0),
		Hostname:     host,
		PaladinBuild: os.Getenv("PALADIN_BUILD"),
	}
}
