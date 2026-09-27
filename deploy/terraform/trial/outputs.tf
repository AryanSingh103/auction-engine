output "healthz_url" {
  value = "http://${aws_lb.main.dns_name}/healthz"
}

output "ecr_repository_url" {
  value = aws_ecr_repository.api.repository_url
}
