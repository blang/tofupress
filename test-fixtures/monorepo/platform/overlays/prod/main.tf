# Prod overlay - references parent and multiple modules
module "base" {
  source = "../../base"
}

module "monitoring" {
  source = "../../../infra/modules/monitoring"
  
  environment = "prod"
  alerting    = true
}

module "storage" {
  source = "../../../infra/modules/storage"
  
  environment = "prod"
  tier        = "premium"
}

module "compute" {
  source = "../../../infra/modules/compute"
  
  environment = "prod"
  size        = "large"
}
