// Package loadgen drives concurrent bidders against the API and then
// verifies that the run measured what it claims to. See docs/decisions/015.
//
// A load test that accidentally measures nothing (requests that never reach
// the server, "accepted" bids that were never written, a hot path that was
// never contended) produces confident, meaningless numbers. So every run
// ends with checks that compare the client's view against the server's
// metrics and the database, and a run that fails them returns an error.
package loadgen

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/AryanSingh103/auction-engine/internal/invariants"
)

// Mode selects what the workers do.
type Mode string

const (
	// ModeBids runs closed-loop bidders against real auctions.
	ModeBids Mode = "bids"
	// ModeHealthz hammers GET /healthz: a control measurement of the
	// harness's own ceiling (network path, HTTP stack, client), with no
	// database work at all.
	ModeHealthz Mode = "healthz"
)

// Config describes one run.
type Config struct {
	Mode       Mode
	APIURL     string // e.g. http://api:8080
	MetricsURL string // e.g. http://api:9091/metrics
	Workers    int    // concurrent bidders (or healthz workers)
	Auctions   int    // bids spread round-robin over this many auctions (1 = one hot auction)
	Warmup     time.Duration
	Duration   time.Duration
	// RaiseSteps: each bid is the known minimum plus 0..RaiseSteps
	// increments, so bidders do not all send the same amount.
	RaiseSteps     int
	RequestTimeout time.Duration
	MaxErrorRate   float64
	Label          string
}

// Report is the outcome of a run, written as JSON for docs/benchmarks.
type Report struct {
	Label     string    `json:"label"`
	Mode      Mode      `json:"mode"`
	Config    Config    `json:"config"`
	StartedAt time.Time `json:"started_at"`

	// Measurement window only (after warmup).
	MeasuredSeconds float64            `json:"measured_seconds"`
	Throughput      float64            `json:"throughput_rps"`
	AcceptedPerSec  float64            `json:"accepted_bids_per_second"`
	Latency         Latency            `json:"latency"`
	LatencyByResult map[string]Latency `json:"latency_by_result"`

	// Whole run, including warmup.
	Requests        int64            `json:"requests_total"`
	Responses       int64            `json:"responses_total"`
	TransportErrors int64            `json:"transport_errors"`
	Results         map[string]int64 `json:"results"`
	LogicalBids     int64            `json:"logical_bids"`
	AcceptedBids    int64            `json:"accepted_bids"`
	UnresolvedBids  int64            `json:"unresolved_bids"`

	Checks []Check `json:"checks"`
}

// Check is one post-run verification.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// Failed returns the checks that did not pass.
func (r Report) Failed() []Check {
	var out []Check
	for _, c := range r.Checks {
		if !c.Passed {
			out = append(out, c)
		}
	}
	return out
}

const (
	startingPrice = 1000
	increment     = 100
	// resolveGrace bounds how long a bidder keeps retrying an unknown
	// outcome after the run ends, so the run always terminates.
	resolveGrace = 30 * time.Second
)

// Run executes one load run. pool is used for setup and the post-run
// database checks (it is not used for load).
func Run(ctx context.Context, cfg Config, pool *pgxpool.Pool) (Report, error) {
	rep := Report{Label: cfg.Label, Mode: cfg.Mode, Config: cfg, StartedAt: time.Now().UTC(),
		Results: map[string]int64{}, LatencyByResult: map[string]Latency{}}

	var auctionIDs, userIDs []int64
	if cfg.Mode == ModeBids {
		var err error
		auctionIDs, userIDs, err = setup(ctx, pool, cfg)
		if err != nil {
			return rep, err
		}
	}

	before, err := scrapeRequestCount(ctx, cfg.MetricsURL)
	if err != nil {
		return rep, fmt.Errorf("scrape server metrics before the run: %w", err)
	}

	client := &http.Client{
		Timeout: cfg.RequestTimeout,
		Transport: &http.Transport{
			// The default keeps only 2 idle connections per host: with more
			// workers than that, connections would be closed and reopened
			// constantly and the benchmark would measure TCP handshakes.
			MaxIdleConns:        cfg.Workers,
			MaxIdleConnsPerHost: cfg.Workers,
			MaxConnsPerHost:     cfg.Workers,
			IdleConnTimeout:     90 * time.Second,
		},
	}
	defer client.CloseIdleConnections()

	r := &runner{cfg: cfg, client: client, auctions: auctionIDs}
	measureFrom := time.Now().Add(cfg.Warmup)
	stopAt := measureFrom.Add(cfg.Duration)

	var wg sync.WaitGroup
	workers := make([]*worker, cfg.Workers)
	for i := range workers {
		w := &worker{r: r, id: i, measureFrom: measureFrom, stopAt: stopAt, results: map[string]int64{},
			latByResult: map[string][]time.Duration{}}
		if cfg.Mode == ModeBids {
			w.user = userIDs[i]
			w.auction = auctionIDs[i%len(auctionIDs)]
			w.knownMin = startingPrice
		}
		workers[i] = w
		wg.Add(1)
		go func() {
			defer wg.Done()
			if cfg.Mode == ModeHealthz {
				w.runHealthz(ctx)
			} else {
				w.runBids(ctx)
			}
		}()
	}
	wg.Wait()

	// Merge per-worker data (each worker wrote only its own, so no locks
	// were needed during the run).
	var all []time.Duration
	byResult := map[string][]time.Duration{}
	for _, w := range workers {
		all = append(all, w.latencies...)
		for k, v := range w.latByResult {
			byResult[k] = append(byResult[k], v...)
		}
		for k, v := range w.results {
			rep.Results[k] += v
		}
		rep.LogicalBids += w.logical
		rep.AcceptedBids += w.accepted
		rep.UnresolvedBids += w.unresolved
	}
	rep.Requests = r.requests.Load()
	rep.Responses = r.responses.Load()
	rep.TransportErrors = r.transportErrors.Load()
	rep.MeasuredSeconds = cfg.Duration.Seconds()
	rep.Throughput = float64(len(all)) / rep.MeasuredSeconds
	for _, w := range workers {
		rep.AcceptedPerSec += float64(w.acceptedMeasured)
	}
	rep.AcceptedPerSec /= rep.MeasuredSeconds
	for k, v := range byResult {
		rep.LatencyByResult[k] = summarize(v)
	}
	rep.Latency = summarize(all)

	after, err := scrapeRequestCount(ctx, cfg.MetricsURL)
	if err != nil {
		return rep, fmt.Errorf("scrape server metrics after the run: %w", err)
	}
	rep.Checks = append(rep.Checks, checkServerSawRequests(after-before, rep.Responses, rep.TransportErrors))
	rep.Checks = append(rep.Checks, checkErrorRate(rep, cfg.MaxErrorRate))
	if cfg.Mode == ModeBids {
		dbChecks, err := checkDatabase(ctx, pool, auctionIDs, rep.AcceptedBids)
		if err != nil {
			return rep, err
		}
		rep.Checks = append(rep.Checks, dbChecks...)
	}
	return rep, nil
}

// runner is shared, read-mostly state plus request counters.
type runner struct {
	cfg      Config
	client   *http.Client
	auctions []int64

	requests, responses, transportErrors atomic.Int64
}

// worker is one closed-loop client: it sends a request, waits for the
// answer, and only then sends the next.
type worker struct {
	r                   *runner
	id                  int
	user, auction       int64
	knownMin            int64
	leading             bool
	measureFrom, stopAt time.Time

	seq                                             int
	logical, accepted, acceptedMeasured, unresolved int64
	results                                         map[string]int64
	latencies                                       []time.Duration
	latByResult                                     map[string][]time.Duration
}

func (w *worker) runHealthz(ctx context.Context) {
	for time.Now().Before(w.stopAt) && ctx.Err() == nil {
		start := time.Now()
		status, _, err := w.do(ctx, http.MethodGet, "/healthz", nil, "")
		w.record(start, resultName(status, "", err), true)
	}
}

func (w *worker) runBids(ctx context.Context) {
	for time.Now().Before(w.stopAt) && ctx.Err() == nil {
		if w.leading {
			// A real bidder who holds the lead waits to be outbid rather
			// than bidding against itself (which can only fail). Watch the
			// auction; these GETs are auxiliary and excluded from the
			// headline latency.
			w.refresh(ctx)
			if w.leading {
				_ = sleep(ctx, 2*time.Millisecond)
			}
			continue
		}
		w.seq++
		w.logical++
		key := fmt.Sprintf("%s-w%d-%d", w.r.cfg.Label, w.id, w.seq)
		amount := w.knownMin + int64(rand.IntN(w.r.cfg.RaiseSteps+1))*increment //nolint:gosec // load shaping, not security
		w.placeBid(ctx, key, amount)
	}
}

// placeBid sends one logical bid and resolves its outcome. A 503, a 500 or
// a transport error means the outcome is UNKNOWN (the bid may have
// committed; open question R14), so the same idempotency key is re-sent
// until the server gives a definitive answer. Counting such a bid as
// "rejected" would make the accepted total disagree with the database.
func (w *worker) placeBid(ctx context.Context, key string, amount int64) {
	body, _ := json.Marshal(map[string]int64{"amount": amount})
	path := fmt.Sprintf("/auctions/%d/bids", w.auction)
	deadline := w.stopAt.Add(resolveGrace)
	for attempt := 0; ; attempt++ {
		start := time.Now()
		status, resp, err := w.do(ctx, http.MethodPost, path, body, key)
		code := errorCode(resp)
		w.record(start, resultName(status, code, err), true)

		switch {
		case status == http.StatusCreated || status == http.StatusOK:
			// 200 is a replay: an earlier attempt of this key committed.
			w.accepted++
			if !start.Before(w.measureFrom) && start.Before(w.stopAt) {
				w.acceptedMeasured++
			}
			w.knownMin = amount + increment
			w.leading = true
			return
		case status == http.StatusConflict && code == "bid_too_low":
			if m := minimumFrom(resp); m > 0 {
				w.knownMin = m
			}
			return
		case status == http.StatusConflict && code == "self_outbid":
			// We believed we were NOT leading (a leading bidder does not
			// bid), yet the server says we are: either another attempt of
			// ours committed, or (optimistic strategy) the server judged a
			// stale snapshot. Refresh and carry on.
			w.refresh(ctx)
			return
		case status >= 400 && status < 500:
			return // definitive rejection (e.g. auction ended)
		}
		// Unknown outcome: retry the same key with a short random backoff.
		if time.Now().After(deadline) || ctx.Err() != nil {
			w.unresolved++
			return
		}
		_ = sleep(ctx, time.Duration(5+rand.IntN(20))*time.Millisecond) //nolint:gosec // jitter
	}
}

// refresh reads the auction to learn the current minimum and whether this
// worker leads.
func (w *worker) refresh(ctx context.Context) {
	start := time.Now()
	status, resp, err := w.do(ctx, http.MethodGet, fmt.Sprintf("/auctions/%d", w.auction), nil, "")
	w.record(start, "get_"+resultName(status, "", err), false)
	var a struct {
		MinimumBid      int64  `json:"minimum_bid"`
		CurrentLeaderID *int64 `json:"current_leader_id"`
	}
	if status != http.StatusOK || json.Unmarshal(resp, &a) != nil {
		return
	}
	if a.MinimumBid > 0 {
		w.knownMin = a.MinimumBid
	}
	w.leading = a.CurrentLeaderID != nil && *a.CurrentLeaderID == w.user
}

// record counts every request and, if it started inside the measurement
// window, keeps its latency. Only primary requests (bid POSTs, or healthz
// GETs in the control mode) feed the headline latency and throughput;
// auxiliary GETs that refresh a bidder's view are reported separately.
func (w *worker) record(start time.Time, result string, primary bool) {
	w.results[result]++
	if start.Before(w.measureFrom) || !start.Before(w.stopAt) {
		return
	}
	d := time.Since(start)
	w.latByResult[result] = append(w.latByResult[result], d)
	if primary {
		w.latencies = append(w.latencies, d)
	}
}

func (w *worker) do(ctx context.Context, method, path string, body []byte, key string) (int, []byte, error) {
	w.r.requests.Add(1)
	req, err := http.NewRequestWithContext(ctx, method, w.r.cfg.APIURL+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
		req.Header.Set("X-User-ID", strconv.FormatInt(w.user, 10))
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := w.r.client.Do(req)
	if err != nil {
		w.r.transportErrors.Add(1)
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	w.r.responses.Add(1)
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, data, nil
}

func resultName(status int, code string, err error) string {
	switch {
	case status == 0 && err != nil:
		return "transport_error"
	case code != "":
		return fmt.Sprintf("%d_%s", status, code)
	default:
		return strconv.Itoa(status)
	}
}

func errorCode(body []byte) string {
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return ""
	}
	return e.Error.Code
}

func minimumFrom(body []byte) int64 {
	var e struct {
		Error struct {
			Minimum int64 `json:"minimum"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &e)
	return e.Error.Minimum
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// setup creates one user per worker and the auctions, all open for longer
// than the run. Each run creates fresh rows, so runs never share an auction.
func setup(ctx context.Context, pool *pgxpool.Pool, cfg Config) (auctions, users []int64, err error) {
	rows, err := pool.Query(ctx, `
		INSERT INTO users (name) SELECT $1 || '-bidder-' || g FROM generate_series(1, $2) g RETURNING id`,
		cfg.Label, cfg.Workers)
	if err != nil {
		return nil, nil, fmt.Errorf("create users: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, nil, err
		}
		users = append(users, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("create users: %w", err)
	}

	open := cfg.Warmup + cfg.Duration + resolveGrace + time.Minute
	rows, err = pool.Query(ctx, `
		WITH item AS (INSERT INTO items (title) VALUES ($1 || ' load item') RETURNING id)
		INSERT INTO auctions (item_id, start_at, end_at, starting_price, min_increment)
		SELECT item.id, clock_timestamp(), clock_timestamp() + $2::interval, $3, $4
		FROM item, generate_series(1, $5)
		RETURNING id`,
		cfg.Label, fmt.Sprintf("%d milliseconds", open.Milliseconds()), startingPrice, increment, cfg.Auctions)
	if err != nil {
		return nil, nil, fmt.Errorf("create auctions: %w", err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, nil, err
		}
		auctions = append(auctions, id)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("create auctions: %w", err)
	}
	return auctions, users, nil
}

var scrapeClient = &http.Client{Timeout: 10 * time.Second}

// scrapeRequestCount returns the server's total http_requests_total across
// all routes and statuses.
func scrapeRequestCount(ctx context.Context, url string) (float64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	// Bounded: http.DefaultClient has no timeout, and a stalled metrics
	// endpoint would otherwise hang the run forever.
	resp, err := scrapeClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("metrics endpoint returned %s", resp.Status)
	}
	return sumMetric(resp.Body, "http_requests_total")
}

// sumMetric adds up every sample of name in Prometheus text format. An
// absent metric counts as 0: a counter vector with no label values yet
// (a server that has served nothing) emits no samples at all. That cannot
// hide a wrong URL, because the "server counted every response" check
// requires a positive delta.
func sumMetric(r io.Reader, name string) (float64, error) {
	var total float64
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, name+"{") && !strings.HasPrefix(line, name+" ") {
			continue
		}
		fields := strings.Fields(line)
		v, err := strconv.ParseFloat(fields[len(fields)-1], 64)
		if err != nil {
			return 0, fmt.Errorf("parse %q: %w", line, err)
		}
		total += v
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

// checkServerSawRequests: every response the client received must have been
// counted by the server. The server may count a few more: requests whose
// response never made it back (client timeout, transport error).
func checkServerSawRequests(serverDelta float64, responses, transportErrors int64) Check {
	extra := int64(serverDelta) - responses
	return Check{
		Name:   "server counted every response the client received",
		Passed: serverDelta > 0 && extra >= 0 && extra <= transportErrors,
		Detail: fmt.Sprintf("server http_requests_total delta %.0f, client responses %d, client transport errors %d",
			serverDelta, responses, transportErrors),
	}
}

func checkErrorRate(rep Report, maxRate float64) Check {
	var bad int64
	for k, v := range rep.Results {
		if k == "transport_error" || strings.HasPrefix(k, "5") {
			bad += v
		}
	}
	rate := 0.0
	if rep.Requests > 0 {
		rate = float64(bad) / float64(rep.Requests)
	}
	return Check{
		Name:   "error rate within limit",
		Passed: rate <= maxRate && rep.UnresolvedBids == 0,
		Detail: fmt.Sprintf("%d of %d requests were 5xx or transport errors (%.3f%%, limit %.3f%%); %d bids left unresolved",
			bad, rep.Requests, 100*rate, 100*maxRate, rep.UnresolvedBids),
	}
}

// checkDatabase compares the client's accepted count with the bids actually
// stored, requires that the price actually moved, and runs every invariant
// check.
func checkDatabase(ctx context.Context, pool *pgxpool.Pool, auctions []int64, accepted int64) ([]Check, error) {
	var stored int64
	var moved int
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM bids WHERE auction_id = ANY($1)),
		       (SELECT count(*) FROM auctions WHERE id = ANY($1) AND current_price > starting_price)`,
		auctions).Scan(&stored, &moved); err != nil {
		return nil, fmt.Errorf("count stored bids: %w", err)
	}
	violations, err := invariants.Run(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("run invariant checks: %w", err)
	}
	var vs []string
	for _, v := range violations {
		vs = append(vs, v.String())
	}
	return []Check{
		{
			Name:   "client accepted count matches bids in the database",
			Passed: stored == accepted,
			Detail: fmt.Sprintf("client counted %d accepted, database has %d", accepted, stored),
		},
		{
			Name:   "load actually moved prices",
			Passed: accepted > 1 && moved > 0,
			Detail: fmt.Sprintf("%d accepted bids; %d of %d auctions above their starting price", accepted, moved, len(auctions)),
		},
		{
			Name:   "invariants hold",
			Passed: len(violations) == 0,
			Detail: fmt.Sprintf("%d violations %s", len(violations), strings.Join(vs, "; ")),
		},
	}, nil
}

// ErrChecksFailed is returned by callers when a run's checks failed.
var ErrChecksFailed = errors.New("load run failed its verification checks; its numbers must not be used")
