# Benchmarks

Every number here is traceable to raw data committed next to it. Numbers come from a laptop VM, not production hardware. Quote them as "on an M3 laptop, in a 4 vCPU colima VM", never as absolute capacity.

---

## 2026-09-24: M2, pessimistic vs optimistic bid locking

Raw data: [`docs/benchmarks/2026-09-24-m2/`](benchmarks/2026-09-24-m2/). It holds one JSON report per run, the loadgen log, and a server-side Prometheus snapshot (`*.server.json`), plus `environment.txt`.

### Environment
- **Host:** Apple M3, 8 cores, 16 GiB, macOS 15.7.7. It's a laptop with other apps open, so treat it as noisy.
- **VM:** colima, **4 vCPU / 6 GiB**, Docker 29.5.2. Postgres, the API **and the load generator all share these 4 vCPU.**
- **Software:**
  - Go 1.27.1 and Postgres 18.6 (`postgres:18.6-alpine3.24`)
  - git commit `151b6e2`
  - image IDs recorded in `environment.txt`
- **Config:**
  - `DB_MAX_CONNS=20`
  - `REQUEST_TIMEOUT=5s`
  - Postgres at stock settings
  - the full schema, including the guard triggers

### Method
- The command is `scripts/bench.sh docs/benchmarks/2026-09-24-m2 30 5 3`, summarized with `scripts/bench_summary.py`.
- Each run: a **fresh database**, then 5s warmup and a **30s measurement window**. The load generator runs **closed-loop** workers inside the compose network (ADR 015).
  - Each worker bids the known minimum plus 0–3 increments.
  - A worker that leads waits (polling with GET) instead of bidding against itself.
- **3 repetitions** per configuration, interleaved (rep → config → strategy). Tables show the **median (min–max)**.
- Every run passed all 5 verification checks: the server saw every request, client-accepted equals database bids, prices moved, invariants held, and errors stayed within the limit.
- This section was **corrected after the M2 adversarial review**, which recomputed every number from the raw JSON and found the original analysis overreached in places. The table values didn't change; their interpretation did.
- **Scenarios:**
  - **hot-wN:** N bidders on **one** auction, which is maximum contention.
  - **spread-wN:** N bidders over N/2 auctions, 2 per auction, which is minimal contention.
- **Latency** is client-side and exact (every sample), for bid POSTs only.
- **"Too-low share"** is the fraction of bid responses that were `409 bid_too_low`.

### Results

| run | reps | throughput req/s | accepted bids/s | p50 ms | p99 ms | p99.9 ms | too-low share | GET share of all requests | lock wait p99 ms (server) | optimistic conflicts | contention 503s |
|---|---|---|---|---|---|---|---|---|---|---|---|
| control-healthz-w50 | 3 | 68048 (66999–79331) | – | 0.50 (0.44–0.52) | 3.78 (3.21–3.83) | 6.3 (5.6–7.0) | – | – | – | – | – |
| pessimistic-hot-w2 | 3 | 515 (510–542) | 515 (510–542) | 0.68 (0.67–0.72) | 1.10 (1.10–1.22) | 4.1 (3.2–4.2) | 0% (0%–0%) | 69% (68%–69%) | 0.5 (0.5–0.5) | – | – |
| optimistic-hot-w2 | 3 | 513 (511–552) | 513 (511–552) | 0.67 (0.67–0.74) | 0.98 (0.97–1.06) | 2.2 (2.0–2.8) | 0% (0%–0%) | 69% (68%–69%) | 0.5 (0.5–0.5) | 0 (0–0) | 0 (0–0) |
| pessimistic-hot-w10 | 3 | 4145 (4041–4207) | 1126 (1083–1146) | 1.74 (1.72–1.76) | 3.28 (3.08–3.68) | 5.5 (4.8–12.4) | 73% (73%–73%) | 35% (35%–35%) | 3.6 (3.5–3.6) | – | – |
| optimistic-hot-w10 | 3 | 3631 (3618–3855) | 919 (904–995) | 1.91 (1.79–1.93) | 7.00 (6.51–7.06) | 13.3 (10.9–26.0) | 75% (74%–75%) | 34% (34%–34%) | 1.7 (1.3–1.8) | 92936 (91392–101355) | 0 (0–0) |
| pessimistic-hot-w50 | 3 | 5855 (5801–5877) | 371 (368–376) | 8.43 (8.42–8.53) | 13.39 (13.16–14.12) | 38.6 (30.1–45.7) | 94% (94%–94%) | 7% (7%–7%) | 7.0 (7.0–7.0) | – | – |
| optimistic-hot-w50 | 3 | 6586 (6485–6939) | 349 (343–368) | 6.13 (5.90–6.21) | 18.48 (17.06–19.20) | 66.2 (35.4–69.8) | 95% (95%–95%) | 7% (7%–7%) | 9.1 (8.0–10.5) | 118694 (117524–124616) | 0 (0–0) |
| pessimistic-hot-w200 | 3 | 6284 (6137–6439) | 114 (112–116) | 30.75 (30.30–31.90) | 45.73 (45.21–49.48) | 71.2 (59.3–89.1) | 98% (98%–98%) | 2% (2%–2%) | 7.0 (6.9–7.0) | – | – |
| optimistic-hot-w200 | 3 | 10336 (8289–10822) | 198 (158–204) | 16.60 (15.67–20.46) | 40.59 (40.23–53.01) | 82.7 (73.9–83.1) | 98% (98%–98%) | 2% (2%–2%) | 7.0 (7.0–12.0) | 48888 (41679–53456) | 0 (0–0) |
| pessimistic-spread-w10 | 3 | 2007 (1750–2107) | 2007 (1750–2107) | 1.18 (0.89–1.21) | 3.34 (1.65–4.43) | 13.1 (4.7–32.0) | 0% (0%–0%) | 71% (71%–71%) | 0.5 (0.5–0.5) | – | – |
| optimistic-spread-w10 | 3 | 2099 (2088–2121) | 2099 (2088–2121) | 1.05 (0.89–1.09) | 2.35 (1.84–2.63) | 5.6 (5.6–7.2) | 0% (0%–0%) | 71% (70%–71%) | 0.5 (0.5–0.5) | 0 (0–0) | 0 (0–0) |
| pessimistic-spread-w50 | 3 | 3109 (2498–3402) | 3109 (2498–3402) | 5.50 (4.81–6.26) | 13.11 (12.24–27.13) | 56.2 (54.0–146.6) | 0% (0%–0%) | 75% (74%–75%) | 1.9 (1.8–2.6) | – | – |
| optimistic-spread-w50 | 3 | 2764 (2235–3583) | 2764 (2235–3583) | 6.15 (4.60–7.37) | 15.60 (11.70–24.71) | 57.4 (30.6–95.2) | 0% (0%–0%) | 75% (74%–75%) | 3.2 (2.6–3.6) | 0 (0–0) | 0 (0–0) |
| pessimistic-spread-w200 | 3 | 3137 (2974–3944) | 3137 (2974–3944) | 19.36 (15.30–19.66) | 46.03 (31.05–79.30) | 95.3 (68.7–312.9) | 0% (0%–0%) | 68% (68%–68%) | 2.1 (1.8–2.3) | – | – |
| optimistic-spread-w200 | 3 | 3257 (2586–3324) | 3257 (2586–3324) | 18.38 (17.64–23.35) | 58.34 (43.51–66.36) | 164.1 (83.8–168.5) | 0% (0%–0%) | 68% (68%–68%) | 3.3 (3.1–3.5) | 0 (0–6) | 0 (0–0) |

\* **Server-side lock wait is coarse, and it measures different things per strategy.**
- Coarse: it comes from Prometheus buckets about 1.9× apart (…, 3.62, 7.01, 13.57 ms…), so "7.0" means "somewhere in 3.6–7.0 ms".
- Different things: under pessimistic it times `SELECT … FOR UPDATE` for **every** bid, doomed ones included. Under optimistic it times the bid **INSERT** (guard trigger lock plus index and foreign-key work), and only for bids that passed the unlocked check.

Don't compare that column across strategies. The client-side percentiles are exact and comparable.

**Server-side snapshot medians (direction only):**

| run | api CPU-seconds | pool empty-acquire waits |
|---|---|---|
| control-healthz-w50 | 50.6 (49.6–52.4) | 0 (0–0) |
| pessimistic-hot-w2 | 9.0 (9.0–9.2) | 0 (0–0) |
| optimistic-hot-w2 | 9.0 (8.3–9.6) | 0 (0–0) |
| pessimistic-hot-w10 | 26.8 (26.4–28.4) | 0 (0–10) |
| optimistic-hot-w10 | 33.7 (32.7–35.2) | 0 (0–0) |
| pessimistic-hot-w50 | 30.9 (28.7–31.4) | 237640 (221908–242888) |
| optimistic-hot-w50 | 37.3 (35.3–38.8) | 376587 (371879–396735) |
| pessimistic-hot-w200 | 31.8 (31.1–34.3) | 231237 (230574–245449) |
| optimistic-hot-w200 | 40.6 (39.6–41.1) | 431531 (352912–464976) |
| pessimistic-spread-w10 | 25.2 (23.0–28.4) | 0 (0–11) |
| optimistic-spread-w10 | 28.0 (21.1–28.5) | 0 (0–0) |
| pessimistic-spread-w50 | 19.9 (15.0–30.2) | 230311 (179838–301351) |
| optimistic-spread-w50 | 29.6 (25.8–30.6) | 330809 (237580–360772) |
| pessimistic-spread-w200 | 29.7 (15.8–32.1) | 352001 (226681–380902) |
| optimistic-spread-w200 | 31.6 (31.1–34.4) | 380727 (326909–425963) |

These vary up to 2× between identical runs, sometimes in the wrong direction. For example, `pessimistic-spread-w50` used 15.0 CPU-s in rep 1 and 30.2 in rep 3 while doing *less* work. I couldn't find the cause because the Prometheus history has since been discarded. Treat these as a direction, not a measurement.

### What the numbers say (and don't)

**Valid comparisons: hot-w10, hot-w50 and hot-w200.** In these runs GETs are 2–35% of requests, bids are genuinely concurrent on one row, and the reps are tight.
- **hot-w10 (moderate contention): pessimistic is better.**
  - 14% more throughput, 23% more accepted bids/s, and p99 of 3.3 vs 7.0 ms.
  - Optimistic logged about 93,000 conflicts in a run with about 32,000 accepted bids: roughly 3 wasted attempts per accepted bid.
  - Its API used more CPU (median 33.7 vs 26.8 CPU-s, about +26%, direction only).
- **hot-w50: mixed.**
  - Optimistic has 12% more throughput and a lower p50 (6.1 vs 8.4 ms).
  - Pessimistic has more accepted bids/s (371 vs 349) and a better p99 (13.4 vs 18.5 ms).
- **hot-w200 (extreme contention, 98% doomed bids): optimistic has more throughput**, +64% req/s and +74% accepted bids/s.
  - **Hypothesis, not established:** the optimistic path rejects doomed bids from an unlocked snapshot, so they never occupy the row lock.
  - Supporting evidence: a doomed bid's median latency was 30.8 ms under pessimistic vs 16.6 ms under optimistic.
  - Confound: at 200 workers, both strategies spend most of that time queueing for one of the **20 pool connections**. Within each strategy, doomed and accepted bids take about the same time, and optimistic actually recorded *more* pool waits.
  - The experiment that would settle it is R15: pessimistic plus an unlocked "too low" pre-check, at 200 workers, and with a larger pool.

**Not valid as bid-path comparisons: hot-w2 and every spread run.** These mostly measure the load generator.
- A bidder that leads doesn't bid. It polls with a 2 ms sleep plus a GET until outbid. In hot-w2 only one bid is ever in flight, so throughput is set by that polling interval, and the two strategies land at 515 vs 513 bids/s.
- In the spread runs each auction has 2 bidders, one of which is always polling. So there is **no lock contention by construction**, and GETs are 68–75% of all requests.
- The large pool-wait counts in the spread runs are partly this GET traffic.
- These runs show the strategies don't differ when nothing contends, and nothing more. Fixing this (a poll-interval flag, and showing the results don't move with it) is noted for the next benchmark round.

**The `/healthz` control (about 68,000 req/s)** shows the load generator can issue far more requests than any bid run needed. It does **not** show the load generator stayed out of the bid path's way. It shares the same 4 vCPU with the API and Postgres, and its own CPU wasn't measured.

**Self-outbid:** none were observed, but this workload **can't** produce the spurious self-outbid that ADR 014 worries about. The load generator never bids while it knows it leads, and a READ COMMITTED read is never older than what the client already knows. The case is untested, not disproven.

### Caveats
- **One machine, one VM:** the load generator competes with the API and Postgres for the same 4 vCPU. Postgres CPU wasn't measured separately.
- **Postgres at stock settings,** with no tuning. The absolute numbers would move with tuning; the comparison between strategies is the point.
- **Closed-loop workers** measure throughput at a fixed concurrency. They are not a model of real users (open-loop arrival rates), and they understate tail latency under overload (coordinated omission).
- **The too-low and GET shares count the whole run,** including warmup and the unknown-outcome resolve phase, not just the 30 s window.
- **Noise:** the `spread` runs vary up to ±30% between repetitions. Differences smaller than the min–max range are not results.

The decision these numbers support is recorded in ADR 016.
