# MyApp - references the account_config module via ../../
module "account_config" {
  source = "../../modules/account_config"

  app_name = "myapp"
}
