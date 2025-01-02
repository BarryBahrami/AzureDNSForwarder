# outputs.tf

output "vm_names" {
  value = azurerm_linux_virtual_machine.dns_forwarder[*].name
}

output "vm_passwords" {
  value     = random_password.password[*].result
  sensitive = true
}

output "vm_private_ips" {
  value = azurerm_network_interface.nic[*].ip_configuration[0].private_ip_address
}