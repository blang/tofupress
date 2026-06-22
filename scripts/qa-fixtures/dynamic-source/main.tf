variable "mod_source" {
  type = string
}
module "dynamic" {
  source = var.mod_source
}
