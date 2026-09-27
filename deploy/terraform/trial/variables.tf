variable "region" {
  description = "AWS region to deploy into."
  type        = string
}

variable "image_tag" {
  description = "Tag of the API image in the stack's ECR repository (the git SHA)."
  type        = string
}

variable "instance_type" {
  description = "EC2 instance type for the single ECS container instance. Must be arm64 (Graviton): the image is built for linux/arm64."
  type        = string
}
