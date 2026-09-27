# 021. Trial deploy shape (M3.5)

## Context
M3.5 rehearses M6: serve `/healthz` on AWS via Terraform, then destroy. Cheap, short-lived, close to M6's shape.

## Decision
- **ECS on EC2, one t4g.small** (Graviton, arm64 image) in an ASG, `launch_type = "EC2"`, **bridge networking** with dynamic host ports. The ALB (HTTP only) health-checks `/healthz`.
- **Default VPC, public subnets, public IP on the instance.** No NAT gateway. The instance SG accepts only the ALB, on the ephemeral range.
- **No Postgres or Redis.** The pools connect lazily, so the URLs point at `.invalid` hosts. `/readyz` returns 503 by design.
- IMDSv2, hop limit 1: containers cannot use the instance role. Local state; 1-day logs.

## Alternatives
- **awsvpc networking:** each task gets its own ENI and SG, but a t4g.small has 3 ENIs, one taken by the instance (2 tasks, so a rolling deploy is tight), and tasks get no public IP. Reconsider in M6.
- **A capacity provider with managed scaling:** needed in M6, but excess for one fixed instance.
- **Fargate:** simpler, but the brief says ECS on EC2.

## Consequences
About $0.07/hour (t4g.small $0.0168, ALB $0.0225 plus LCUs, public IPv4s, EBS). No TLS, no remote state: M6 adds both.
