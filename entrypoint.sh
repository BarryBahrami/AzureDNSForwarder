#!/bin/sh
set -e

# The /config directory is normally a host bind mount, so its ownership is
# whatever user created it on the host. Re-own it for the runtime user so
# the app can write the config, lock file, and audit log.
chown -R dnsfwd:dnsfwd /config 2>/dev/null || true

# Re-own unbound directories as well in case the mount changed.
chown -R dnsfwd:dnsfwd /etc/unbound /var/run 2>/dev/null || true

# Run the dnsforwarderd binary as the unprivileged dnsfwd user. File
# capabilities on the binary still grant NET_BIND_SERVICE / NET_RAW.
exec su-exec dnsfwd /usr/local/bin/dnsforwarderd "$@"
