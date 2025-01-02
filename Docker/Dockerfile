# Use a lightweight Alpine base image
FROM alpine:latest

# Update the package list and upgrade installed packages
RUN apk update && apk upgrade

# Install dnsmasq
RUN apk add --no-cache dnsmasq

# Copy the dnsmasq configuration file
COPY dnsmasq.conf /etc/dnsmasq.conf

# Expose DNS ports for both UDP and TCP
EXPOSE 53/udp
EXPOSE 53/tcp

# Run dnsmasq in the foreground without any DHCP services
CMD ["dnsmasq", "--no-daemon"]
