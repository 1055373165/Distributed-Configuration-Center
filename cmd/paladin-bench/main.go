// paladin-bench — load-testing CLI for PaladinCore.
//
// Subcommands:
//
//	run    — execute one scenario, emit summary / JSON / Markdown.
//	report — aggregate a directory of JSON results into one Markdown report.
//
// Quickstart:
//
//	paladin-bench run --scenario=write_only --addrs=127.0.0.1:8080
//	paladin-bench run --scenario=read_only  --addrs=127.0.0.1:8080,127.0.0.1:8081,127.0.0.1:8082
//	paladin-bench run --scenario=mixed --read-percent=95 --concurrency=64 --duration=30s
//	paladin-bench report --in=bench/results --out=bench/results/summary.md
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"paladin-core/bench"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "run":
		runCmd(os.Args[2:])
	case "report":
		reportCmd(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		os.Exit(1)
	}
}

func runCmd(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	scenario := fs.String("scenario", "write_only", "one of: write_only, read_only, mixed")
	addrsStr := fs.String("addrs", "127.0.0.1:8080", "comma-separated host:port list (writes go to the first)")
	tenant := fs.String("tenant", "bench", "tenant name")
	namespace := fs.String("namespace", "prod", "namespace name")
	numKeys := fs.Int("num-keys", 1000, "size of the key pool")
	valueSize := fs.Int("value-size", 64, "PUT body size in bytes")
	readPercent := fs.Int("read-percent", 95, "for scenario=mixed: percent of reads (0-100)")
	concurrency := fs.Int("concurrency", 32, "concurrent workers")
	duration := fs.Duration("duration", 30*time.Second, "measurement window")
	warmup := fs.Duration("warmup", 3*time.Second, "discarded warm-up time")
	rpsCap := fs.Int("rps", 0, "cap aggregate RPS (0=unlimited)")
	timeout := fs.Duration("timeout", 10*time.Second, "per-request timeout")
	jsonOut := fs.String("json", "", "write result JSON to this path")
	reportOut := fs.String("report", "", "write Markdown report to this path")
	fs.Parse(args)

	addrs := parseAddrs(*addrsStr)
	if len(addrs) == 0 {
		fatal("at least one --addrs entry is required")
	}

	cfg := bench.ScenarioConfig{
		Addrs:     addrs,
		Tenant:    *tenant,
		Namespace: *namespace,
		NumKeys:   *numKeys,
		ValueSize: *valueSize,
		Timeout:   *timeout,
	}

	var sc bench.Scenario
	switch *scenario {
	case "write_only":
		sc = bench.NewWriteOnly(cfg)
	case "read_only":
		sc = bench.NewReadOnly(cfg)
	case "mixed":
		if *readPercent < 0 || *readPercent > 100 {
			fatal("--read-percent must be in 0..100")
		}
		sc = bench.NewMixed(cfg, *readPercent)
	default:
		fatal("unknown scenario: %s (expected write_only|read_only|mixed)", *scenario)
	}

	// Graceful interrupt: cancel the run but still emit partial results.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n[bench] interrupt, finishing current run...")
		cancel()
	}()

	fmt.Printf("[bench] scenario=%s conc=%d duration=%s warmup=%s rps_cap=%d addrs=%v\n",
		sc.Name(), *concurrency, *duration, *warmup, *rpsCap, addrs)

	res, err := bench.Run(ctx, sc, bench.LoadConfig{
		Concurrency: *concurrency,
		Duration:    *duration,
		WarmUp:      *warmup,
		RPSCap:      *rpsCap,
	})
	if err != nil {
		fatal("run: %v", err)
	}

	printSummary(res)

	if *jsonOut != "" {
		if err := bench.SaveJSON(res, *jsonOut); err != nil {
			fatal("save json: %v", err)
		}
		fmt.Printf("[bench] wrote %s\n", *jsonOut)
	}
	if *reportOut != "" {
		f, err := os.Create(*reportOut)
		if err != nil {
			fatal("create report: %v", err)
		}
		defer f.Close()
		title := "PaladinCore Benchmark — " + res.Scenario
		if err := bench.RenderMarkdown(f, title, []*bench.Result{res}); err != nil {
			fatal("render: %v", err)
		}
		fmt.Printf("[bench] wrote %s\n", *reportOut)
	}
}

func reportCmd(args []string) {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	in := fs.String("in", "bench/results", "directory containing *.json result files")
	out := fs.String("out", "-", "output path (- for stdout)")
	title := fs.String("title", "PaladinCore Benchmark Sweep", "report title")
	fs.Parse(args)

	files, err := filepath.Glob(filepath.Join(*in, "*.json"))
	if err != nil {
		fatal("glob: %v", err)
	}
	sort.Strings(files)

	var results []*bench.Result
	for _, f := range files {
		r, err := bench.LoadJSON(f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[bench] skipping %s: %v\n", f, err)
			continue
		}
		results = append(results, r)
	}

	var w io.Writer = os.Stdout
	if *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			fatal("create: %v", err)
		}
		defer f.Close()
		w = f
	}
	if err := bench.RenderMarkdown(w, *title, results); err != nil {
		fatal("render: %v", err)
	}
	if *out != "-" {
		fmt.Fprintf(os.Stderr, "[bench] wrote %s (%d results)\n", *out, len(results))
	}
}

// parseAddrs splits and trims a comma-separated endpoint list.
func parseAddrs(s string) []bench.Addr {
	parts := strings.Split(s, ",")
	out := make([]bench.Addr, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, bench.Addr(p))
		}
	}
	return out
}

func printSummary(r *bench.Result) {
	fmt.Printf("\nscenario   : %s\n", r.Scenario)
	fmt.Printf("duration   : %s\n", r.Duration)
	fmt.Printf("count      : %d\n", r.Count)
	fmt.Printf("rps        : %.1f\n", r.RPS)
	fmt.Printf("errors     : %d  (class: %v)\n", r.Errors, r.StatusClass)
	fmt.Printf("latency    : min=%v mean=%v max=%v\n",
		r.Latency.Min, r.Latency.Mean, r.Latency.Max)
	fmt.Printf("             p50=%v p95=%v p99=%v p99.9=%v\n",
		r.Latency.P50, r.Latency.P95, r.Latency.P99, r.Latency.P999)
}

func fatal(f string, a ...interface{}) {
	fmt.Fprintf(os.Stderr, "error: "+f+"\n", a...)
	os.Exit(1)
}

func usage() {
	fmt.Fprintln(os.Stderr, `paladin-bench — PaladinCore load testing

Commands:
  run       execute one scenario
  report    aggregate a directory of JSON results into Markdown

Run flags:
  --scenario       write_only | read_only | mixed    (default write_only)
  --addrs          comma-separated host:port list    (default 127.0.0.1:8080)
  --tenant         tenant name                       (default bench)
  --namespace      namespace name                    (default prod)
  --num-keys       key pool size                     (default 1000)
  --value-size     PUT body size in bytes            (default 64)
  --read-percent   (mixed) percent reads             (default 95)
  --concurrency    worker goroutines                 (default 32)
  --duration       measurement window                (default 30s)
  --warmup         discarded warm-up time            (default 3s)
  --rps            cap aggregate RPS (0=unlimited)   (default 0)
  --timeout        per-request timeout               (default 10s)
  --json PATH      write result JSON
  --report PATH    write Markdown report

Report flags:
  --in DIR         directory of *.json result files  (default bench/results)
  --out PATH       output path (- for stdout)        (default -)
  --title STR      report title

Examples:
  paladin-bench run --scenario=write_only --concurrency=64 --duration=30s
  paladin-bench run --scenario=read_only  --addrs=127.0.0.1:8080,127.0.0.1:8081,127.0.0.1:8082
  paladin-bench run --scenario=mixed --read-percent=95 --duration=60s --report=out.md
  paladin-bench report --in=bench/results --out=bench/results/summary.md`)
}
