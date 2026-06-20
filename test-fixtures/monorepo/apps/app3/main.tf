# App3 - deep nesting test, references multiple modules
module "network" {
  source = "../../infra/modules/network"
  
  app_name = "app3"
}

module "compute" {
  source = "../../infra/modules/compute"
  
  environment = "app3"
  size        = "small"
}

module "storage" {
  source = "../../infra/modules/storage"
  
  environment = "app3"
  tier        = "premium"
}

module "monitoring" {
  source = "../../infra/modules/monitoring"
  
  environment = "app3"
}
