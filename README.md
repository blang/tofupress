# tofupress

A CLI that takes an OpenTofu/Terraform root module, recursively resolves all referenced modules, and bundles everything into a single self-contained artifact (tar.gz or OCI Terraform module) — no external module sources left to resolve at runtime.

## Why

Terraform's module system is great for sharing code, but it creates a dependency tree of arbitrary depth that consumers must resolve at init time. The usual alternative — vendoring all modules into the repository — is tedious to manage and clutters git history, especially when those modules recursively pull in submodules of their own. **tofupress** lets you keep using modules the way Terraform intended while producing a single, hermetic artifact at release time. As a bonus, the recursive condensing step can deduplicate modules that appear multiple times in the tree.
