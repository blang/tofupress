# App1 - references infra modules
module "network" {
  source = "../../infra/modules/network"

  app_name = "app1"
}

module "compute" {
  source = "../../infra/modules/compute"

  environment = "app1"
  size        = "medium"
}

module "monitoring" {
  source = "../../infra/modules/monitoring"

  environment = "app1"
}
