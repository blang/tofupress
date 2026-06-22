# Staging environment - similar to prod but different config
module "network" {
  source = "../../modules/network"

  app_name = "staging"
}

module "compute" {
  source = "../../modules/compute"

  environment = "staging"
  size        = "large"
}

module "storage" {
  source = "../../modules/storage"

  environment = "staging"
  tier        = "standard"
}

module "monitoring" {
  source = "../../modules/monitoring"

  environment = "staging"
  alerting    = false
}
