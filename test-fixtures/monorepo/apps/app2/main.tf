# App2 - references infra modules with different config
module "network" {
  source = "../../infra/modules/network"

  app_name = "app2"
}

module "storage" {
  source = "../../infra/modules/storage"

  environment = "app2"
  tier        = "standard"
}

module "shared" {
  source = "../../infra/modules/shared"

  environment = "app2"
}
