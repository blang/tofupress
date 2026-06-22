# Storage module - references shared
module "shared" {
  source = "../shared"

  environment = "storage"
}

variable "environment" {
  type = string
}

variable "tier" {
  type    = string
  default = "standard"
}

output "storage_config" {
  value = {
    env  = var.environment
    tier = var.tier
  }
}
