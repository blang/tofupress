# Monitoring module - references shared
module "shared" {
  source = "../shared"
  
  environment = "monitoring"
}

variable "environment" {
  type = string
}

variable "alerting" {
  type    = bool
  default = false
}

output "monitoring_config" {
  value = {
    env      = var.environment
    alerting = var.alerting
    shared   = module.shared.config
  }
}
