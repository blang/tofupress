# Account Configuration module
variable "app_name" {
  type = string
}

output "config" {
  value = "configured: ${var.app_name}"
}
