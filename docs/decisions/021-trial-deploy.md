# 021. Trial deploy shape (M3.5)

## Context
M3.5 is a rehearsal: get `/healthz` served on AWS with Terraform, then destroy it, so M6 is not the first contact with ECS, IAM and ALBs. It should be cheap, short-lived and close to the M6 shape.

## Decision
- **ECS on EC2, one t4g.small** (Graviton, arm64 image) in an ASG, `launch_type = "EC2"`, **bridge networking** with dynamic host ports. The ALB (HTTP only) health-checks `/healthz`.
- **Default VPC, public subnets, public IP on the instance.** No NAT gateway. The instance SG accepts only the ALB, on the ephemeral range.
- **No Postgres or Redis.** The pools connect lazily, so the URLs point at `.invalid` hosts. `/readyz` returns 503 by design.
- IMDSv2 with hop limit 1, so containers cannot use the instance role. Local state. Logs are kept for 1 day.

## Alternatives
- **awsvpc networking:** each task gets its own ENI and SG, but a t4g.small has only 2 ENIs (1 task), and tasks have no public IP. Reconsider in M6.
- **A capacity provider with managed scaling:** needed in M6, but excess for one fixed instance.
- **Fargate:** simpler, but the brief says ECS on EC2.
- **A sidecar Postgres:** that would be more than "/healthz".

## Consequences
The cost is about $0.07/hour (t4g.small $0.0168, ALB $0.0225 plus LCUs, public IPv4s, EBS). There is no TLS and no remote state, and M6 must add both.
