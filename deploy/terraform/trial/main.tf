locals {
  name = "auction-engine-trial"
}

# ---- network: the account's default VPC -----------------------------------
# Public subnets with an internet gateway, so the instance reaches ECR and
# the ECS control plane without a NAT gateway (about $30/month, R10). M6
# builds its own VPC.

data "aws_vpc" "default" {
  default = true
}

data "aws_subnets" "default" {
  filter {
    name   = "vpc-id"
    values = [data.aws_vpc.default.id]
  }
  filter {
    name   = "default-for-az"
    values = ["true"]
  }
}

resource "aws_security_group" "alb" {
  name        = "${local.name}-alb"
  description = "Public HTTP to the ALB"
  vpc_id      = data.aws_vpc.default.id
}

resource "aws_vpc_security_group_ingress_rule" "alb_http" {
  security_group_id = aws_security_group.alb.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 80
  to_port           = 80
}

# Bridge networking maps the container to a random host port from the
# Linux ephemeral range, so the ALB may reach any port in it, and only on
# instances in the instance security group.
resource "aws_vpc_security_group_egress_rule" "alb_to_instances" {
  security_group_id            = aws_security_group.alb.id
  referenced_security_group_id = aws_security_group.instance.id
  ip_protocol                  = "tcp"
  from_port                    = 32768
  to_port                      = 65535
}

resource "aws_security_group" "instance" {
  name        = "${local.name}-instance"
  description = "ECS container instance: dynamic ports from the ALB only, no SSH"
  vpc_id      = data.aws_vpc.default.id
}

resource "aws_vpc_security_group_ingress_rule" "instance_from_alb" {
  security_group_id            = aws_security_group.instance.id
  referenced_security_group_id = aws_security_group.alb.id
  ip_protocol                  = "tcp"
  from_port                    = 32768
  to_port                      = 65535
}

# Outbound HTTPS to ECR, S3 (image layers), CloudWatch Logs and ECS.
resource "aws_vpc_security_group_egress_rule" "instance_https" {
  security_group_id = aws_security_group.instance.id
  cidr_ipv4         = "0.0.0.0/0"
  ip_protocol       = "tcp"
  from_port         = 443
  to_port           = 443
}

# ---- image registry -------------------------------------------------------

resource "aws_ecr_repository" "api" {
  name                 = local.name
  image_tag_mutability = "IMMUTABLE" # a tag (git SHA) always means the same image
  force_delete         = true        # destroy must succeed with images inside
}

# ---- IAM ------------------------------------------------------------------

data "aws_iam_policy_document" "ec2_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ec2.amazonaws.com"]
    }
  }
}

# The ECS agent on the instance uses this role to register with the
# cluster and poll for tasks.
resource "aws_iam_role" "instance" {
  name               = "${local.name}-instance"
  assume_role_policy = data.aws_iam_policy_document.ec2_assume.json
}

resource "aws_iam_role_policy_attachment" "instance_ecs" {
  role       = aws_iam_role.instance.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonEC2ContainerServiceforEC2Role"
}

resource "aws_iam_instance_profile" "instance" {
  name = "${local.name}-instance"
  role = aws_iam_role.instance.name
}

data "aws_iam_policy_document" "ecs_tasks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
    condition {
      test     = "StringEquals"
      variable = "aws:SourceAccount"
      values   = [data.aws_caller_identity.current.account_id]
    }
  }
}

data "aws_caller_identity" "current" {}

# Execution role: what ECS itself uses to pull the image and ship logs.
# The API makes no AWS calls, so there is no task role.
resource "aws_iam_role" "execution" {
  name               = "${local.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_tasks_assume.json
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# ---- compute: one ECS container instance ----------------------------------

resource "aws_ecs_cluster" "main" {
  name = local.name
}

# AWS publishes the current ECS-optimized AMI ID in SSM, so no AMI ID is
# hard-coded and the plan always picks up the latest.
data "aws_ssm_parameter" "ecs_ami" {
  name = "/aws/service/ecs/optimized-ami/amazon-linux-2023/arm64/recommended/image_id"
}

resource "aws_launch_template" "ecs" {
  name_prefix   = "${local.name}-"
  image_id      = data.aws_ssm_parameter.ecs_ami.value
  instance_type = var.instance_type

  iam_instance_profile {
    arn = aws_iam_instance_profile.instance.arn
  }

  network_interfaces {
    associate_public_ip_address = true # reach ECR/ECS without a NAT gateway
    security_groups             = [aws_security_group.instance.id]
  }

  # IMDSv2 only. A hop limit of 1 means packets from bridge-mode containers
  # (one extra hop) cannot reach the metadata service, so the API container
  # cannot borrow the instance role's credentials.
  metadata_options {
    http_tokens                 = "required"
    http_put_response_hop_limit = 1
  }

  user_data = base64encode(<<-EOT
    #!/bin/bash
    echo "ECS_CLUSTER=${aws_ecs_cluster.main.name}" >> /etc/ecs/ecs.config
  EOT
  )
}

resource "aws_autoscaling_group" "ecs" {
  name                = local.name
  min_size            = 1
  max_size            = 1
  desired_capacity    = 1
  vpc_zone_identifier = data.aws_subnets.default.ids

  launch_template {
    id      = aws_launch_template.ecs.id
    version = aws_launch_template.ecs.latest_version
  }

  # ASG instances don't inherit the provider's default_tags.
  tag {
    key                 = "Project"
    value               = "auction-engine"
    propagate_at_launch = true
  }
  tag {
    key                 = "Milestone"
    value               = "m3.5-trial"
    propagate_at_launch = true
  }
  tag {
    key                 = "Name"
    value               = local.name
    propagate_at_launch = true
  }
}

# ---- load balancer --------------------------------------------------------

resource "aws_lb" "main" {
  name               = local.name
  load_balancer_type = "application"
  security_groups    = [aws_security_group.alb.id]
  subnets            = data.aws_subnets.default.ids
  # idle_timeout stays at the 60s default, below HTTP_IDLE_TIMEOUT (75s).
}

resource "aws_lb_target_group" "api" {
  name        = local.name
  vpc_id      = data.aws_vpc.default.id
  target_type = "instance" # bridge mode: targets are instance:dynamic-port
  protocol    = "HTTP"
  port        = 8080 # placeholder; ECS registers each task's real host port

  # /healthz, not /readyz: this trial has no Postgres or Redis, so /readyz
  # is 503 by design. In M6 the choice matters: a readiness check that
  # fails on a database outage makes the ALB drain every task at once.
  health_check {
    path                = "/healthz"
    matcher             = "200"
    interval            = 10
    timeout             = 5
    healthy_threshold   = 2
    unhealthy_threshold = 3
  }

  # When ECS stops a task (deploy, scale-in, service delete), it
  # deregisters it, waits this long, then sends SIGTERM. The default 300s
  # would make every deploy and destroy slow. This does NOT happen when
  # the ASG terminates the instance (replacement, refresh, health check):
  # ECS is not told, so the task gets SIGTERM while the ALB still routes
  # to it. Observed in the trial: SIGTERM 26s before deregistration. M6
  # needs a capacity provider with managed draining (R18).
  deregistration_delay = 15
}

resource "aws_lb_listener" "http" {
  load_balancer_arn = aws_lb.main.arn
  port              = 80
  protocol          = "HTTP"

  default_action {
    type             = "forward"
    target_group_arn = aws_lb_target_group.api.arn
  }
}

# ---- the API service ------------------------------------------------------

resource "aws_cloudwatch_log_group" "api" {
  name              = "/ecs/${local.name}"
  retention_in_days = 1
}

resource "aws_ecs_task_definition" "api" {
  family                   = local.name
  requires_compatibilities = ["EC2"]
  network_mode             = "bridge"
  execution_role_arn       = aws_iam_role.execution.arn

  runtime_platform {
    operating_system_family = "LINUX"
    cpu_architecture        = "ARM64"
  }

  container_definitions = jsonencode([{
    name      = "api"
    image     = "${aws_ecr_repository.api.repository_url}:${var.image_tag}"
    essential = true
    cpu       = 256
    memory    = 256 # hard limit (MiB): the container is OOM-killed above it

    portMappings = [{
      containerPort = 8080
      hostPort      = 0 # dynamic: lets a new task start beside the old one during a deploy
      protocol      = "tcp"
    }]

    # SIGTERM to SIGKILL. Must stay above SHUTDOWN_TIMEOUT (15s), which
    # bounds the whole graceful shutdown (ADR 003).
    stopTimeout = 20

    # Every variable is required (ADR 001). DATABASE_URL and REDIS_URL point
    # at the reserved .invalid TLD (RFC 2606), which never resolves: the
    # pgx pool and Redis client connect lazily, so the API starts and serves
    # /healthz, while /readyz honestly reports 503.
    environment = [
      { name = "HTTP_ADDR", value = ":8080" },
      { name = "METRICS_ADDR", value = ":9091" }, # not mapped, so unreachable from outside
      { name = "LOG_LEVEL", value = "info" },
      { name = "SHUTDOWN_TIMEOUT", value = "15s" },
      { name = "HTTP_READ_HEADER_TIMEOUT", value = "5s" },
      { name = "HTTP_READ_TIMEOUT", value = "10s" },
      { name = "HTTP_WRITE_TIMEOUT", value = "10s" },
      { name = "HTTP_IDLE_TIMEOUT", value = "75s" },
      { name = "REQUEST_TIMEOUT", value = "5s" },
      { name = "DATABASE_URL", value = "postgres://trial:trial@db.invalid:5432/trial?sslmode=disable" },
      { name = "DB_MAX_CONNS", value = "5" },
      { name = "DB_IDLE_IN_TX_TIMEOUT", value = "5s" },
      { name = "REDIS_URL", value = "redis://redis.invalid:6379/0" },
      { name = "REDIS_TIMEOUT", value = "200ms" },
      { name = "AUCTION_CACHE_TTL", value = "10s" },
      { name = "WS_SEND_BUFFER", value = "64" },
      { name = "WS_MAX_CONNECTIONS", value = "100" },
      { name = "WS_PING_INTERVAL", value = "20s" },
      { name = "WS_WRITE_TIMEOUT", value = "5s" },
      { name = "WS_SYNC_INTERVAL", value = "5s" },
      { name = "RATE_LIMIT_BIDS_PER_SECOND", value = "10" },
      { name = "RATE_LIMIT_BID_BURST", value = "20" },
      { name = "BID_LOCKING", value = "pessimistic" },
    ]

    logConfiguration = {
      logDriver = "awslogs"
      options = {
        awslogs-group         = aws_cloudwatch_log_group.api.name
        awslogs-region        = var.region
        awslogs-stream-prefix = "api"
      }
    }
  }])
}

resource "aws_ecs_service" "api" {
  name            = local.name
  cluster         = aws_ecs_cluster.main.id
  task_definition = aws_ecs_task_definition.api.arn
  desired_count   = 1
  launch_type     = "EC2"

  health_check_grace_period_seconds = 10

  load_balancer {
    target_group_arn = aws_lb_target_group.api.arn
    container_name   = "api"
    container_port   = 8080
  }

  # `terraform apply` returns only once the task is running and healthy
  # behind the ALB, so a green apply means the deploy actually worked.
  wait_for_steady_state = true

  # The listener must exist before ECS registers targets.
  depends_on = [aws_lb_listener.http, aws_iam_role_policy_attachment.execution]
}
