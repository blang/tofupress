module "a" {
  source = "./child1"
  for_each = toset(["a", "b"])
}
module "b" {
  source = "./child2"
  count = 2
}
