# variables.tf

variable "resource_group_name" {
  type        = string
  description = "Name of the resource group where the VNet exists."
}

variable "location" {
  type        = string
  description = "Location where resources will be created."
}

variable "subnet_id" {
  type        = string
  description = "ID of the existing Subnet where the VM will be deployed."
}

variable "vm_size" {
  type    = string
  default = "Standard_B1s"
}

variable "vm_count" {
  type        = number
  default     = 1
  description = "Number of VMs to create."
}

variable "vm_availability_zones" {
  type    = list(string)
  default = ["1"]
  #i.e. you could also use ["1","2"] for a VM in zone 1 and 2.
  #make sure vm_count is equal to or less than the number of zones you provide.
  description = "List of availability zones for VM placement."
}

variable "static_ips" {
  type        = list(string)
  description = "List of static IP addresses to assign to VMs"
}