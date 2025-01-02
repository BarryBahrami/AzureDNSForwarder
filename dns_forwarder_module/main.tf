# main.tf

resource "azurerm_network_security_group" "nsg" {
  name                = "nsg-dns-forwarder"
  location            = var.location
  resource_group_name = var.resource_group_name

  security_rule {
    name                       = "AllowDNS"
    priority                   = 1000
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "53"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "AllowPing"
    priority                   = 1010 # Set priority after DNS rule but before deny all
    direction                  = "Inbound"
    access                     = "Allow"
    protocol                   = "Icmp"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "AllowOutboundAzureDNS"
    priority                   = 1000
    direction                  = "Outbound"
    access                     = "Allow"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "168.63.129.16"
  }

  security_rule {
    name                       = "AllowOutboundContainerPull"
    priority                   = 1010
    direction                  = "Outbound"
    access                     = "Allow"
    protocol                   = "Tcp"
    source_port_range          = "*"
    destination_port_ranges    = ["80", "443"]
    source_address_prefix      = "*"
    destination_address_prefix = "Internet"
  }


  security_rule {
    name                       = "DenyAllInbound"
    priority                   = 4096
    direction                  = "Inbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }

  security_rule {
    name                       = "DenyAllOutbound"
    priority                   = 4096
    direction                  = "Outbound"
    access                     = "Deny"
    protocol                   = "*"
    source_port_range          = "*"
    destination_port_range     = "*"
    source_address_prefix      = "*"
    destination_address_prefix = "*"
  }
}

resource "azurerm_linux_virtual_machine" "dns_forwarder" {
  count               = var.vm_count
  name                = "AzureDNSForwarder-${count.index + 1}"
  resource_group_name = var.resource_group_name
  location            = var.location
  size                = var.vm_size # Lean VM size for Ubuntu
  admin_username      = "dnsadminuser"
  network_interface_ids = [
    azurerm_network_interface.nic[count.index].id
  ]

  admin_password                  = random_password.password[count.index].result
  disable_password_authentication = false

  os_disk {
    caching              = "ReadWrite"
    storage_account_type = "Standard_LRS"
  }

  source_image_reference {
    publisher = "canonical"
    offer     = "ubuntu-24_04-lts"
    sku       = "server"
    version   = "latest"
  }

  zone = var.vm_availability_zones[count.index]
}

resource "random_password" "password" {
  count            = var.vm_count
  length           = 32
  special          = true
  override_special = "!@#$%^&*()-_=+[]{}|;:,.<>?"
}

resource "azurerm_network_interface" "nic" {
  count               = var.vm_count
  name                = "nic-dns-forwarder-${count.index + 1}"
  location            = var.location
  resource_group_name = var.resource_group_name

  ip_configuration {
    name                          = "ipconfig"
    subnet_id                     = var.subnet_id
    private_ip_address_allocation = "Static"
    private_ip_address            = var.static_ips[count.index]
  }
}

resource "azurerm_network_interface_security_group_association" "nsg_association" {
  count                     = var.vm_count
  network_interface_id      = azurerm_network_interface.nic[count.index].id
  network_security_group_id = azurerm_network_security_group.nsg.id
}

# In dns_forwarder_module/main.tf

resource "azurerm_virtual_machine_extension" "docker" {
  count                = var.vm_count
  name                 = "docker-install"
  virtual_machine_id   = azurerm_linux_virtual_machine.dns_forwarder[count.index].id
  publisher            = "Microsoft.Azure.Extensions"
  type                 = "CustomScript"
  type_handler_version = "2.0"

  protected_settings = <<SETTINGS
    {
      "script": "${base64encode(file("${path.module}/install_docker_and_run_container.sh"))}"
    }
  SETTINGS

  settings = <<SETTINGS
    {
      "fileUris": []
    }
  SETTINGS
}