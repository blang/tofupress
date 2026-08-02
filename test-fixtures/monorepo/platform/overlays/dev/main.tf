# Dev overlay — references parent platform/base and cross-references infra modules
# Tests the most extreme ../ nesting in the monorepo (../../base and ../../../infra/...)

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
