# Power-cycles one host of an instance group. It only runs when you invoke it
# by name:
#
#   terraform apply -invoke=action.k0rdent_instance_power.cycle
data "k0rdent_instances" "training" {
  instance_group = k0rdent_instance_group.training.id
}

action "k0rdent_instance_power" "cycle" {
  config {
    instance_id = data.k0rdent_instances.training.ids[0]
    action      = "powerCycle"
  }
}

# The platform accepts the action and doesn't report when the machine gets
# there. A failure it learns of later shows on the instance.
output "last_power_action_error" {
  value = data.k0rdent_instances.training.instances[0].last_power_action_error
}

# An instance outside the provider's scope needs its scope named here too.
action "k0rdent_instance_power" "elsewhere" {
  config {
    instance_id = "inst-0123456789abcdef0123456789abcdef"
    action      = "gracefulShutdown"
    region      = "eu-west-1"
    project     = "cp-research"
  }
}
