// Command loadgen fires concurrent bidders at the API, then verifies that
// the run measured something real before reporting any numbers.
//
// It is a one-shot developer tool, so it is configured with flags (the
// services' env-only rule is for long-running processes). The JSON report
// goes to stdout (or --out), the human summary to stderr. The exit status
// is non-zero if any verification check failed, so a bad run cannot be
// mistaken for a good one in scripts. See docs/decisions/015.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/loadgen"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var cfg loadgen.Config
	var mode, dbURL, out string
	flag.StringVar(&mode, "mode", "bids", "bids (closed-loop bidders) or healthz (control: harness ceiling)")
	flag.StringVar(&cfg.APIURL, "api-url", "", "API base URL, e.g. http://api:8080 (required)")
	flag.StringVar(&cfg.MetricsURL, "metrics-url", "", "API metrics URL, e.g. http://api:9091/metrics (required)")
	flag.StringVar(&dbURL, "database-url", "", "Postgres URL for setup and post-run checks (required)")
	flag.IntVar(&cfg.Workers, "workers", 10, "concurrent bidders")
	flag.IntVar(&cfg.Auctions, "auctions", 1, "auctions to spread bidders over (1 = a single hot auction)")
	flag.DurationVar(&cfg.Warmup, "warmup", 5*time.Second, "warmup before measuring")
	flag.DurationVar(&cfg.Duration, "duration", 30*time.Second, "measurement window")
	flag.IntVar(&cfg.RaiseSteps, "raise-steps", 3, "each bid is the known minimum plus 0..N increments")
	flag.DurationVar(&cfg.RequestTimeout, "request-timeout", 10*time.Second, "client timeout per HTTP request")
	flag.Float64Var(&cfg.MaxErrorRate, "max-error-rate", 0.01, "fail the run if more than this fraction of requests are 5xx or transport errors")
	flag.StringVar(&cfg.Label, "label", "", "run label; also prefixes idempotency keys and seeded rows (default: loadgen-<unix time>)")
	flag.StringVar(&out, "out", "-", "write the JSON report here ('-' for stdout)")
	flag.Parse()

	cfg.Mode = loadgen.Mode(mode)
	if cfg.Label == "" {
		cfg.Label = fmt.Sprintf("loadgen-%d", time.Now().Unix())
	}
	switch {
	case cfg.APIURL == "" || cfg.MetricsURL == "" || dbURL == "":
		return errors.New("--api-url, --metrics-url and --database-url are required")
	case cfg.Mode != loadgen.ModeBids && cfg.Mode != loadgen.ModeHealthz:
		return fmt.Errorf("--mode must be bids or healthz, got %q", mode)
	case cfg.Workers < 1 || cfg.Auctions < 1 || cfg.Auctions > cfg.Workers:
		return errors.New("need --workers >= 1 and 1 <= --auctions <= --workers")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	fmt.Fprintf(os.Stderr, "loadgen %s: %s mode, %d workers, %d auction(s), %s warmup + %s measured\n",
		cfg.Label, cfg.Mode, cfg.Workers, cfg.Auctions, cfg.Warmup, cfg.Duration)
	rep, err := loadgen.Run(ctx, cfg, pool)
	if err != nil {
		return err
	}
	if err := writeReport(rep, out); err != nil {
		return err
	}
	printSummary(rep)

	if failed := rep.Failed(); len(failed) > 0 {
		return fmt.Errorf("%w (%d failed)", loadgen.ErrChecksFailed, len(failed))
	}
	return nil
}

func writeReport(rep loadgen.Report, out string) error {
	f := os.Stdout
	if out != "-" {
		var err error
		f, err = os.Create(out) //nolint:gosec // path chosen by the operator running the tool
		if err != nil {
			return fmt.Errorf("create report: %w", err)
		}
		defer func() { _ = f.Close() }()
	}
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func printSummary(rep loadgen.Report) {
	l := rep.Latency
	fmt.Fprintf(os.Stderr, "\nthroughput  %.1f req/s   accepted %.1f bids/s   (%d primary requests in %.0fs)\n",
		rep.Throughput, rep.AcceptedPerSec, l.Count, rep.MeasuredSeconds)
	fmt.Fprintf(os.Stderr, "latency ms  p50 %.2f  p90 %.2f  p99 %.2f  p99.9 %.2f  max %.2f  mean %.2f\n",
		l.P50, l.P90, l.P99, l.P999, l.Max, l.Mean)
	keys := make([]string, 0, len(rep.Results))
	for k := range rep.Results {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Fprint(os.Stderr, "results    ")
	for _, k := range keys {
		fmt.Fprintf(os.Stderr, " %s=%d", k, rep.Results[k])
	}
	fmt.Fprintln(os.Stderr)
	for _, c := range rep.Checks {
		mark := "PASS"
		if !c.Passed {
			mark = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "[%s] %s: %s\n", mark, c.Name, c.Detail)
	}
}
