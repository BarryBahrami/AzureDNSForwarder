# Azure DNS Forwarder

This is a very simple and lean Azure DNS forwarder, designed for situations when you need a VPN to resolve an internal Azure IP, etc. It receives DNS requests via UDP/TCP and forwards them to the well-known Azure DNS IP 168.63.129.16. If you need to resolve IP addresses for your private endpoints or private DNS zones, this is your solution. It is based on dnsmasq.

With Azure Private Resolver costing nearly $200 per month for each instance, it seems impractical not to set up a simple DNS forwarder in an inexpensive (~$12 per month) and locked-down VM. That's what this solution offers. Even if your usage is somewhat heavier and requires a VM with more resources, it will still be less expensive than ~$200 per month—$2160/year vs. $144/year per server.

This DNS forwarder is contained in a simple Docker container, and a Terraform provisioning module is included to set it up in a VM for you. Alternatively, you can run the container wherever you see fit and expose UDP/TCP port 53.

The NSG setup with this module allows outbound 80/443 access so that the VM can provision itself. This is not required after setup and can be set to deny if you prefer. There is no harm in leaving it as-is.

It would not be difficult to expand on this and install WireGuard, doubling as a P2P VPN gateway for your on-premises network and saving the cost of Azure VPN as well. If there is interest, I'll add it to this project.

To run just the docker container:

```sh
docker run -d --name azurednsforwarder -p 53:53/udp -p 53:53/tcp barrybahrami/azurednsforwarder:latest
```

## _Terraform Provisioning Module_

This module can be run as-is with the included main.tf file (in the root), or you can import the module into your own project. It will spin up one or more inexpensive VMs, install Docker, and deploy the container. It will also wrap the VM in a Network Security Group (NSG) so that only DNS traffic can enter. Outbound traffic is open to DNS and ports 80/443, allowing the container to be pulled. SSH access is locked down, and the password to the VM is randomized. Record it if you want, but you shouldn't need to log in—it's just a simple DNS forwarder. You can control IP access using the NSG. I recommend placing your NSG (if any) on the subnet to prevent your code from inadvertently overwriting the one placed by this module, which could potentially open up security holes. This module does not set up a public IP on the VM(s). You shouldn't need one if traffic is coming in via VPN.

The module requires parameters as listed below. Be sure the static IP addresses are from the subnet. Yes, static IP. You don't want your DNS server on a dynamic IP, do you?

```sh
provider "azurerm" {
  subscription_id = "########-####-####-####-############"
  features {}
}

resource_group_name   = "your-resource-group-name"
location              = "your-location"
subnet_id             = "/subscriptions/your-subscription-id/resourceGroups/your-resource-group/providers/Microsoft.Network/virtualNetworks/your-vnet-name/subnets/your-subnet-name"
vm_size = "Standard_B1s" # This is a tiny VM, which is probably fine.  Upgrade if heavy usage.
vm_count              = 1
vm_availability_zones = ["1"]
static_ips            = ["10.0.0.10"]
```

You can also do something like:

```sh
vm_count              = 2
  vm_availability_zones = ["1","2"]
  static_ips            = ["10.0.0.10","10.0.0.11"]
```

And in a production setup, I think you should.

If you are using AzureVPN then you should edit the DNS server in your VPN P2S config and point it to your new VM(s).  I mean, really you should get away from AzureVPN.  But if you must, it goes like this:

```sh
<dnsservers>
  <dnsserver>10.0.0.10</dnsserver>
</dnsservers>
```

The Dockerfile used to create the container is available for review in the respective folder.

Feel free to reach out with any questions.

LinkedIn: https://www.linkedin.com/in/barrybahrami/
Email: BarryBahrami at gmail

If this helped you then please consider donating to the San Diego Web Cam.
Bitcoin: bc1q0a5sf8q0j90qedndrmvgulv0rwxlfhc8rgk8c9

And watch on YouTube!  SunDiegoLive.com

Thank you