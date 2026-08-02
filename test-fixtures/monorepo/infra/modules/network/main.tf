# Network module — references sibling modules within the same monorepo
# Demonstrates cross-module dependencies within a monorepo structure

variable "app_name" {
  description = "Name of the application using this network"
  type        = string
}

module "shared" {
  source = "../shared"
}

output "vpc_id" {
  value = "vpc-${var.app_name}"
}
