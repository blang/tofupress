# Account Configuration - references shared module via ../../
module "shared" {
  source = "../../shared"
  env = var.app_name
}

variable "app_name" {
  type = string
}
