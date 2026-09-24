# 006. Schema migrations with goose, as a separate one-shot step

## Context
The brief allows golang-migrate or goose. The schema must be versioned, reproducible in tests, and applied safely when several processes start at once.

## Decision
- **goose v3.** Migrations are SQL files embedded in the binary (`embed.FS`). goose runs each migration in its own transaction, so a failure leaves nothing half-applied.
- **Separate `cmd/migrate` binary.** Migrations never run at API startup. Compose runs migrate as a one-shot service that the API waits on. M6 will run it as a one-off ECS task.
- **Postgres advisory lock** (`lock.NewPostgresSessionLocker`). Two concurrent runs serialize: verified, one run applied 7 migrations and the other applied 0.

## Alternatives considered
- **golang-migrate:** a failed migration leaves the schema marked "dirty", and that needs manual repair.
- **Migrating at API startup:** N instances race, and a bad migration crash-loops the whole service instead of failing one visible job.

## Consequences
Deploys have two steps (migrate, then roll the API). Every migration must be backward-compatible with the API version still running during a rollout. Every migration has a Down section, tested manually with up/down/up.
