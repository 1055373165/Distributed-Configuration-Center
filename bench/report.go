package bench

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// RenderMarkdown writes a human-readable report for one or more Results.
//
// Layout:
//   1. Environment (from the first result — assumes all were run together).
//   2. Summary table, one row per result.
//   3. Per-scenario details with full latency breakdown.
//
// When comparing historical baselines, pipe two different result sets through
// this function and diff the Markdown with `diff -u`.
func RenderMarkdown(w io.Writer, title string, results []*Result) error {
	fmt.Fprintf(w, "# %s\n\n", title)
	if len(results) == 0 {
		fmt.Fprintln(w, "_No results._")
		return nil
	}

	// Stable ordering so the same inputs always produce the same output
	// (helpful for diffs).
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Scenario != results[j].Scenario {
			return results[i].Scenario < results[j].Scenario
		}
		return results[i].Concurrency < results[j].Concurrency
	})

	env := results[0].Env
	fmt.Fprintln(w, "## Environment")
	fmt.Fprintf(w, "- Timestamp: %s\n", env.Timestamp.Format(time.RFC3339))
	fmt.Fprintf(w, "- Host: %s\n", env.Hostname)
	fmt.Fprintf(w, "- OS/Arch: %s/%s\n", env.GOOS, env.GOARCH)
	fmt.Fprintf(w, "- CPUs: %d (GOMAXPROCS=%d)\n", env.NumCPU, env.GOMAXPROCS)
	fmt.Fprintf(w, "- Go: %s\n", env.Go)
	if env.PaladinBuild != "" {
		fmt.Fprintf(w, "- Paladin build: %s\n", env.PaladinBuild)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "## Summary")
	fmt.Fprintln(w, "| Scenario | Conc | Dur | RPS | P50 | P95 | P99 | P99.9 | Max | Errors |")
	fmt.Fprintln(w, "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
	for _, r := range results {
		fmt.Fprintf(w, "| %s | %d | %s | %.0f | %s | %s | %s | %s | %s | %d |\n",
			r.Scenario,
			r.Concurrency,
			formatDur(r.Duration),
			r.RPS,
			formatDur(r.Latency.P50),
			formatDur(r.Latency.P95),
			formatDur(r.Latency.P99),
			formatDur(r.Latency.P999),
			formatDur(r.Latency.Max),
			r.Errors,
		)
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "## Details")
	for _, r := range results {
		fmt.Fprintf(w, "### %s (concurrency=%d, duration=%s)\n\n",
			r.Scenario, r.Concurrency, formatDur(r.Duration))
		fmt.Fprintf(w, "- Total requests: %d\n", r.Count)
		fmt.Fprintf(w, "- RPS: %.1f\n", r.RPS)
		fmt.Fprintf(w, "- Errors: %d\n", r.Errors)
		fmt.Fprintf(w, "- Status class: %s\n", statusClassSummary(r.StatusClass))
		fmt.Fprintln(w, "- Latency:")
		fmt.Fprintf(w, "  - min=%s mean=%s max=%s\n",
			formatDur(r.Latency.Min), formatDur(r.Latency.Mean), formatDur(r.Latency.Max))
		fmt.Fprintf(w, "  - p50=%s p90=%s p95=%s p99=%s p99.9=%s\n",
			formatDur(r.Latency.P50), formatDur(r.Latency.P90),
			formatDur(r.Latency.P95), formatDur(r.Latency.P99),
			formatDur(r.Latency.P999))
		fmt.Fprintln(w)
	}
	return nil
}

// formatDur emits a human-friendly duration string (µs / ms / s).
func formatDur(d time.Duration) string {
	switch {
	case d >= time.Second:
		return fmt.Sprintf("%.2fs", d.Seconds())
	case d >= time.Millisecond:
		return fmt.Sprintf("%.2fms", float64(d)/float64(time.Millisecond))
	case d >= time.Microsecond:
		return fmt.Sprintf("%.0fµs", float64(d)/float64(time.Microsecond))
	default:
		return fmt.Sprintf("%dns", d.Nanoseconds())
	}
}

func statusClassSummary(c [6]uint64) string {
	names := []string{"err", "1xx", "2xx", "3xx", "4xx", "5xx"}
	var parts []string
	for i, n := range c {
		if n > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", names[i], n))
		}
	}
	if len(parts) == 0 {
		return "(none)"
	}
	return strings.Join(parts, " ")
}
