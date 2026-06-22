# Platform base — shared configuration for all environments
# This module demonstrates the monorepo resolution pattern:
# When bundled from a subdirectory using the // syntax:
#   tofupress resolve ./monorepo//infra/environments/prod
# TofuPress resolves all ../ references relative to the monorepo root.

module "network" {
  source = "../../infra/modules/network"

  app_name = "platform-base"
}

module "shared" {
  source = "../../infra/modules/shared"
}
