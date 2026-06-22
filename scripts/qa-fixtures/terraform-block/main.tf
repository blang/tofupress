terraform {
  required_version = ">= 1.0"
  backend "s3" {
    bucket = "test"
  }
}
module "child" {
  source = "./child"
}
