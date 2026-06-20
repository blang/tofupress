# Dev overlay - references parent and sibling
module "base" {
  source = "../../base"
}

module "monitoring" {
  source = "../../../infra/modules/monitoring"
  
  environment = "dev"
}

module "storage" {
  source = "../../../infra/modules/storage"
  
  environment = "dev"
  tier        = "standard"
}
