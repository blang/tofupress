# Compute module - references sibling modules
module "shared" {
  source = "../shared"
  
  environment = "compute"
}

module "storage" {
  source = "../storage"
  
  environment = "compute"
  tier        = "standard"
}

variable "environment" {
  type = string
}

variable "size" {
  type    = string
  default = "medium"
}

output "compute_config" {
  value = {
    env     = var.environment
    size    = var.size
    storage = module.storage.storage_config
  }
}
