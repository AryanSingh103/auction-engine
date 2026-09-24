# auction-engine: project brief

Paste this into Claude Code as your first message. Also keep it at `docs/PROJECT_BRIEF.md` in the repo so future sessions can re-read it.

---

I'm building a portfolio project called auction-engine and I want to work through it with you over several weeks. Read this entire brief before doing anything.

## What it is

A real-time auction engine written in Go, themed as storage unit auctions. The theme is cosmetic. The actual point of the project is distributed systems engineering: concurrency correctness under contention, at-least-once messaging with idempotency, backpressure, and real deployed infrastructure.

Module path: github.com/AryanSingh103/auction-engine

## Who I am and how I want you to work with me

I'm a CS student with strong Python/FastAPI and ML experience and solid full-stack skills. This is my first Go project and my first serious backend systems project. I have no Go, Kafka, Terraform or AWS experience yet.

Non-negotiable rules:

1. Build in small slices. Never generate more than one coherent component at a time, then stop and let me read it.  
     
2. I commit constantly, after every small change, edit or addition, never in big batches. So after each individual logical change, stop and give me the exact command to run, for example: `git add . && git commit -m "add auctions table migration"` If you just wrote three separate things, that is three commits, not one. Do not move on to the next change until you've told me to commit the current one.  
     
3. Explain every design decision as you make it, including what you rejected and why. If you choose pessimistic locking, tell me what optimistic locking would have done differently and when it would be the better choice.  
     
4. Never leave me with code I can't explain. After each slice, ask me one question that tests whether I actually understood it. If I answer badly, explain again before moving on.  
     
5. Do not scaffold the whole project structure up front. Add files as they become necessary.  
     
6. If I ask for something that's a bad idea, say so directly.  
     
7. Record every significant decision in `docs/decisions/NNN-short-title.md`: context, decision, alternatives considered, consequences. Under 200 words each.  
     
8. Assume I will be interviewed in depth about every line of this.

## Rules about honesty

These matter more than anything else in this brief.

- Never tell me something works unless you actually ran it. Run the code, run the tests, and show me the real output.  
- Never make a failing test pass by weakening the assertion, adding a sleep, skipping the test, or lowering a threshold. If a test fails, either the code is wrong or the test is wrong. Tell me which, and why.  
- When something fails, explain the cause before proposing a fix. No shotgun debugging.  
- If a benchmark number looks suspiciously good, be suspicious. Verify the load test is actually exercising what it claims to. A load test that accidentally measures nothing is a very common and very embarrassing failure.  
- If you are unsure about something, say so instead of guessing confidently.

## Verification and review

- Before starting each milestone, use plan mode and show me the plan.  
    
- At the end of each milestone, spawn a subagent to review the work adversarially, in the voice of a skeptical senior backend engineer. Its brief: hunt for race conditions, concurrency bugs, missing error handling, unhandled failure modes, resource leaks, and any path by which the stated invariants could be violated. It should try to construct a specific interleaving of events that breaks something. Have it report findings ranked by severity. Do not defend the code. Fix what is real, and tell me what you dismissed and why.  
    
- Also at the end of each milestone, spawn a second subagent to read that milestone's actual diff and generate 5 interview questions from it, at the level a senior engineer would ask in a technical screen. Give me the questions WITHOUT the answers. I'll answer them, then you tell me where I was wrong or vague. This is the main mechanism for closing my knowledge gaps, so do not skip it.

## Definition of done for a milestone

A milestone is not complete until all of these are true:

- All tests pass, and `go test -race ./...` is clean  
- Where applicable, the invariant checker runs green after a load test  
- Every significant decision is written up in `docs/decisions/`  
- `docs/benchmarks.md` records any numbers produced, with the date, machine specs, and the exact config used, so the numbers are traceable and I can quote them honestly on a resume  
- `CLAUDE.md` is updated if anything structural changed  
- Everything is committed  
- The adversarial review subagent has run and its real findings are fixed

## Stack (already decided, do not re-litigate)

- Go, latest stable version  
- HTTP API: net/http with chi, or Gin. You choose and justify it.  
- PostgreSQL as the single source of truth, pgx driver, migrations via golang-migrate or goose  
- Redis for caching, rate limiting, and pub/sub fan-out between API instances  
- Kafka for async work. Use Redpanda locally since it's much lighter than Kafka in Docker. Client: segmentio/kafka-go or franz-go, you choose.  
- WebSockets for live browser updates  
- Docker Compose for local development, everything containerized from the start  
- ALL configuration from environment variables, never hardcoded, from the very first commit. No localhost strings buried in the code.  
- Prometheus and Grafana for metrics  
- Terraform with AWS (ECS on EC2, RDS, S3, ALB) for deployment, later  
- GitHub Actions: tests running in CI from milestone 1, deployment added at milestone 6  
- Frontend: one plain HTML file with vanilla JS. Deliberately ugly. Spend no effort here.

## Domain model

users; items (a storage unit: title, description, photo keys); auctions (item, start\_at, end\_at, starting\_price, min\_increment, current\_price, current\_leader\_id, status); bids (auction, user, amount, idempotency\_key, created\_at); invoices (auction, winner, amount, status); outbox (event rows written in the same transaction as the state change that produced them).

## The invariants. These ARE the spec. Everything else is support.

1. Exactly one winner per auction, ever.  
2. The winner is the highest bidder.  
3. Every accepted bid exceeds the previous accepted bid by at least min\_increment.  
4. No bid is ever accepted after the auction has closed.  
5. Exactly one charge per closed auction, even if the settlement message is delivered many times.  
6. No bid exists without its outbox event, and no outbox event exists without its bid.  
7. All of the above hold while processes are being killed mid-operation.

Enforce these with database constraints and explicit mechanisms, not with hopeful application logic. Push whatever you can into the schema.

## Mechanisms I specifically want implemented

- Bid path in a single Postgres transaction using `SELECT ... FOR UPDATE` on the auction row, with all validation performed while holding the lock  
- Idempotency keys on bid creation and on settlement  
- Transactional outbox: events written to Postgres in the same transaction as the state change, then published to Kafka by a separate poller using `SELECT ... FOR UPDATE SKIP LOCKED`  
- Kafka messages keyed by auction\_id so ordering is guaranteed per auction  
- Anti-snipe: a bid inside the final 10 seconds extends end\_at by 10 seconds  
- Auction closing handled by a worker holding a Postgres advisory lock (simple leader election), which re-checks end\_at under the same row lock the bid path uses. The database, never a server clock, is the authority on ordering.  
- WebSocket fan-out with a per-connection buffered channel, dropping slow clients rather than blocking the whole room  
- Redis token bucket rate limiting, returning 429 with Retry-After  
- Load shedding when Kafka consumer lag exceeds a threshold  
- A deliberately flaky fake payment service (10% failure rate, occasional 5 second hang) so timeouts, jittered exponential backoff and a circuit breaker have something real to handle  
- Graceful shutdown that drains in-flight work

## Testing, starting at milestone 1, not bolted on at the end

- Table-driven unit tests  
- Integration tests using testcontainers against real Postgres, Redis and Redpanda  
- `go test -race` in CI  
- A load generator in Go firing N concurrent bidders at a single auction  
- An invariant checker that queries the database after a run and asserts every invariant listed above  
- Fault injection: kill workers and API instances mid-run, then re-assert all invariants  
- A fuzz test replaying randomly ordered bid sequences

## Milestones. Do not jump ahead.

0. Repo, Go module, multi-stage Dockerfile, docker compose with Postgres, /healthz endpoint, structured logging with slog, env-based config, Makefile.  
     
1. Schema and migrations. The bid path with row locking. An integration test firing 1000 concurrent bids that proves no lost update. GitHub Actions running tests on every push.  
     
2. Prometheus metrics, Grafana dashboard, load generator, first honest latency and throughput numbers. Benchmark pessimistic vs optimistic locking and write up the measured difference.  
     
3. WebSocket fan-out, Redis caching, Redis pub/sub across multiple API instances, rate limiting.

3.5. A throwaway trial deploy: minimal Terraform to get just /healthz running on AWS, so the real deploy later isn't a cliff. Then `terraform destroy`. Keep this short, half a day at most.

4. Redpanda, transactional outbox, settlement consumer, flaky payment simulator, retries with jittered backoff, circuit breaker, dead letter queue.  
     
5. Closer worker, advisory lock leader election, anti-snipe extension, provable correctness of the close-versus-bid race.  
     
6. Full Terraform for AWS, GitHub Actions deployment pipeline, real deploy.  
     
7. Chaos testing, README with architecture diagram and benchmark graphs, design doc.

## What to do right now

Do not write application code yet. Instead:

1. Ask me about anything above that is genuinely ambiguous. Maximum five questions.  
     
2. Write `CLAUDE.md` capturing the stack, the working rules (especially the commit-after-every-change rule and the honesty rules), the invariants, and the definition of done, so future sessions have this context without me re-pasting it.  
     
3. Propose the file list for milestone 0 and wait for my approval.