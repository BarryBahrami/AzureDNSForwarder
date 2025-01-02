# root/main.tf

terraform {
  required_providers {
    azurerm = {
      source  = "hashicorp/azurerm"
      version = "~> 4.13.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.0"
    }
  }
}

provider "azurerm" {
  subscription_id = "########-####-####-####-############"
  features {}
}

provider "random" {}

module "dns_forwarder_module" {
  source = "./dns_forwarder_module"

  resource_group_name = "your-resource-group-name"
  location            = "your-location" # i.e. "westus3", etc
  subnet_id           = "/subscriptions/your-subscription-id/resourceGroups/your-resource-group/providers/Microsoft.Network/virtualNetworks/your-vnet-name/subnets/your-subnet-name"
  vm_size             = "Standard_B1s" # This is a tiny VM, which is probably fine.  Upgrade if heavy usage.

  # If you get an error, it's probably right here.  Count should equal or be less than the number of items in AZ and IP lists.
  vm_count              = 1
  vm_availability_zones = ["1"]
  static_ips            = ["10.0.0.10"] # Example for one VM, adjust for the number of VMs. You don't want DNS on dynamic IP's, do you?

  /*
  You can also do something like:

  vm_count              = 2
  vm_availability_zones = ["1","2"]
  static_ips            = ["10.0.0.10","10.0.0.11"]

  And in a production setup, I think you should.

  */
}
