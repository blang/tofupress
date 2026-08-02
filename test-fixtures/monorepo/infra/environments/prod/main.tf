# Prod environment - main entry point with deep references
module "network" {
  source = "../../modules/network"

  app_name = "prod"
}

module "compute" {
  source = "../../modules/compute"

  environment = "prod"
  size        = "xlarge"
}

module "storage" {
  source = "../../modules/storage"

  environment = "prod"
  tier        = "premium"
}

module "monitoring" {
  source = "../../modules/monitoring"

  environment = "prod"
  alerting    = true
}

module "platform_base" {
  source = "../../../platform/base"
}
