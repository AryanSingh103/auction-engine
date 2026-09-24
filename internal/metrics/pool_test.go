package metrics

import (
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The pool connects lazily, so a pool pointed at nothing still has valid
// statistics; that is enough to test the collector's shape. Live values
// under load are checked end to end in milestone 2's smoke benchmark.
func TestPoolCollector(t *testing.T) {
	cfg, err := pgxpool.ParseConfig("postgres://nobody@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatal(err)
	}
	cfg.MaxConns = 7
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	m := New()
	m.RegisterPool(pool)

	expected := `
# HELP db_pool_max_conns Configured maximum pool size (DB_MAX_CONNS).
# TYPE db_pool_max_conns gauge
db_pool_max_conns 7
# HELP db_pool_acquired_conns Connections currently checked out of the pool.
# TYPE db_pool_acquired_conns gauge
db_pool_acquired_conns 0
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(expected), "db_pool_max_conns", "db_pool_acquired_conns"); err != nil {
		t.Error(err)
	}
	// Metric names, help text and types must pass Prometheus's own lint.
	problems, err := testutil.GatherAndLint(m.Registry(), "db_pool_acquires_total", "db_pool_empty_acquires_total", "db_pool_max_conns")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		t.Errorf("lint: %s: %s", p.Metric, p.Text)
	}
}
