# Network module - references sibling modules
module "shared" {
  source = "../shared"

  environment = "network"
}

module "monitoring" {
  source = "../monitoring"

  environment = "network"
}

variable "app_name" {
  type = string
}

output "network_config" {
  value = {
    app    = var.app_name
    shared = module.shared.config
  }
}
