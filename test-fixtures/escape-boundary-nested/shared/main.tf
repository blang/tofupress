# Shared module
variable "env" {
  type = string
}

output "shared_output" {
  value = "shared: ${var.env}"
}
