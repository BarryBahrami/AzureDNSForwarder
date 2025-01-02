#!/bin/bash

# Update package lists
sudo apt-get update

# Install necessary packages for Docker installation
sudo apt-get install -y apt-transport-https ca-certificates curl software-properties-common

# Add Docker's official GPG key
curl -fsSL https://download.docker.com/linux/ubuntu/gpg | sudo apt-key add -

# Add Docker repository to APT sources
sudo DEBIAN_FRONTEND=noninteractive add-apt-repository "deb [arch=amd64] https://download.docker.com/linux/ubuntu $(lsb_release -cs) stable" -y

# Update package lists again after adding the Docker repo
sudo apt-get update

# Install Docker
sudo apt-get install -y docker-ce

# Pull the container from Docker Hub
sudo docker pull barrybahrami/azurednsforwarder

# Get the machine's hostname and ffixup the hosts file or we break local DNS and can't start the container.
HOSTNAME=$(hostname)

# Fix /etc/hosts by inserting the hostname to 127.0.0.1 if not already present
if ! grep -q "$HOSTNAME" /etc/hosts; then
    sudo sed -i "1i 127.0.0.1   localhost $HOSTNAME" /etc/hosts
fi

# Stop and disable systemd-resolved
sudo systemctl stop systemd-resolved
sudo systemctl disable systemd-resolved

# Run the Docker container with automatic restart
sudo docker run -d --restart=always --name azurednsforwarder -p 53:53/udp -p 53:53/tcp barrybahrami/azurednsforwarder
