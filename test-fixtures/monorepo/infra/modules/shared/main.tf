# Shared module - leaf module, no dependencies
variable "environment" {
  type = string
}

output "config" {
  value = {
    env      = var.environment
    shared   = true
    features = ["logging", "metrics", "tracing"]
  }
}
