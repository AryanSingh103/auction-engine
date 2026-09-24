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
- **Scenarios:**
  - **hot-wN:** N bidders on **one** auction, which is maximum contention.
  - **spread-wN:** N bidders over N/2 auctions, 2 per auction, which is minimal contention.
- **Latency** is client-side and exact (every sample), for bid POSTs only.
- **"Too-low share"** is the fraction of bid responses that were `409 bid_too_low`.

### Results

| run | reps | throughput req/s | accepted bids/s | p50 ms | p99 ms | p99.9 ms | too-low share | lock wait p99 ms (server)* | optimistic conflicts | contention 503s |
|---|---|---|---|---|---|---|---|---|---|---|
| control-healthz-w50 | 3 | 68048 (66999–79331) | – | 0.50 (0.44–0.52) | 3.78 (3.21–3.83) | 6.3 (5.6–7.0) | – | – | – | – |
| pessimistic-hot-w2 | 3 | 515 (510–542) | 515 (510–542) | 0.68 (0.67–0.72) | 1.10 (1.10–1.22) | 4.1 (3.2–4.2) | 0% | 0.5 | – | – |
| optimistic-hot-w2 | 3 | 513 (511–552) | 513 (511–552) | 0.67 (0.67–0.74) | 0.98 (0.97–1.06) | 2.2 (2.0–2.8) | 0% | 0.5 | 0 | 0 |
| pessimistic-hot-w10 | 3 | 4145 (4041–4207) | 1126 (1083–1146) | 1.74 (1.72–1.76) | 3.28 (3.08–3.68) | 5.5 (4.8–12.4) | 73% | 3.6 | – | – |
| optimistic-hot-w10 | 3 | 3631 (3618–3855) | 919 (904–995) | 1.91 (1.79–1.93) | 7.00 (6.51–7.06) | 13.3 (10.9–26.0) | 75% | 1.7 | 92936 (91392–101355) | 0 |
| pessimistic-hot-w50 | 3 | 5855 (5801–5877) | 371 (368–376) | 8.43 (8.42–8.53) | 13.39 (13.16–14.12) | 38.6 (30.1–45.7) | 94% | 7.0 | – | – |
| optimistic-hot-w50 | 3 | 6586 (6485–6939) | 349 (343–368) | 6.13 (5.90–6.21) | 18.48 (17.06–19.20) | 66.2 (35.4–69.8) | 95% | 9.1 | 118694 (117524–124616) | 0 |
| pessimistic-hot-w200 | 3 | 6284 (6137–6439) | 114 (112–116) | 30.75 (30.30–31.90) | 45.73 (45.21–49.48) | 71.2 (59.3–89.1) | 98% | 7.0 | – | – |
| optimistic-hot-w200 | 3 | 10336 (8289–10822) | 198 (158–204) | 16.60 (15.67–20.46) | 40.59 (40.23–53.01) | 82.7 (73.9–83.1) | 98% | 7.0 | 48888 (41679–53456) | 0 |
| pessimistic-spread-w10 | 3 | 2007 (1750–2107) | 2007 (1750–2107) | 1.18 (0.89–1.21) | 3.34 (1.65–4.43) | 13.1 (4.7–32.0) | 0% | 0.5 | – | – |
| optimistic-spread-w10 | 3 | 2099 (2088–2121) | 2099 (2088–2121) | 1.05 (0.89–1.09) | 2.35 (1.84–2.63) | 5.6 (5.6–7.2) | 0% | 0.5 | 0 | 0 |
| pessimistic-spread-w50 | 3 | 3109 (2498–3402) | 3109 (2498–3402) | 5.50 (4.81–6.26) | 13.11 (12.24–27.13) | 56.2 (54.0–146.6) | 0% | 1.9 | – | – |
| optimistic-spread-w50 | 3 | 2764 (2235–3583) | 2764 (2235–3583) | 6.15 (4.60–7.37) | 15.60 (11.70–24.71) | 57.4 (30.6–95.2) | 0% | 3.2 | 0 | 0 |
| pessimistic-spread-w200 | 3 | 3137 (2974–3944) | 3137 (2974–3944) | 19.36 (15.30–19.66) | 46.03 (31.05–79.30) | 95.3 (68.7–312.9) | 0% | 2.1 | – | – |
| optimistic-spread-w200 | 3 | 3257 (2586–3324) | 3257 (2586–3324) | 18.38 (17.64–23.35) | 58.34 (43.51–66.36) | 164.1 (83.8–168.5) | 0% | 3.3 | 0 (0–6) | 0 |

\* **Server-side quantiles are coarse.** They come from Prometheus histogram buckets roughly 1.9× apart (…, 3.62, 7.01, 13.57 ms…). A value of exactly 7.0 means "somewhere in 3.6–7.0 ms". Use them for direction only. The client-side percentiles are exact.

### What the numbers say
1. **The harness isn't the bottleneck.** The `/healthz` control reached about 68,000 req/s on the same path, 6× more than the best bid throughput. The bid numbers measure the bid path, not the load generator or the network.
2. **With low contention the strategies are indistinguishable.** In `hot-w2` and all `spread` runs the medians sit inside each other's min–max ranges. Any difference there is noise, and I don't claim one.
3. **With moderate contention (hot-w10), pessimistic wins:** 14% more throughput, 23% more accepted bids/s, and p99 3.3ms vs 7.0ms. Optimistic records about 93,000 conflicts in a run that accepted about 32,000 bids, so each accepted bid costs about 3 wasted attempts, and the retries show up as tail latency. The API also burned about 30% more CPU (35 vs 27 CPU-seconds, from `api_cpu_seconds` in the snapshots).
4. **With extreme contention (hot-w200), optimistic wins throughput** (+64% req/s, +74% accepted/s, p50 halved), with a similar p99. **But the reason isn't that it's "optimistic".** 98% of the bids in this run are *doomed* (too low). The optimistic path rejects them from an unlocked snapshot. That's safe because prices only rise (ADR 014). The pessimistic path makes every doomed bid queue for the row lock just to be told no. The lock is the serialization point, and pessimistic wastes it on bids that can't win. At w200 both strategies also queue for **database connections**: 200 workers share a 20-connection pool, and the snapshots show 231k (pessimistic) and 465k (optimistic) waits for a free connection. So that latency is pool queueing on top of lock queueing.
5. **No `self_outbid` responses in any run, for either strategy.** ADR 014 predicted that stale optimistic snapshots *could* produce spurious self-outbids. It's possible in principle, but it wasn't observed at this load.
6. **No retry exhaustion:** `contention` 503s were 0 in every run, so the 10-attempt cap was never reached.

### Caveats
- **One machine, one VM:** the load generator competes with the API and Postgres for the same 4 vCPU. Postgres CPU wasn't measured separately.
- **Postgres at stock settings,** with no tuning. The absolute numbers would move with tuning; the comparison between strategies is the point.
- **Closed-loop workers** measure throughput at a fixed concurrency. They are not a model of real users (open-loop arrival rates), and they can understate tail latency under overload (coordinated omission).
- **Noise:** the `spread` runs vary up to ±30% between repetitions. Differences smaller than the min–max range are not results.

The decision these numbers support is recorded in ADR 016.
