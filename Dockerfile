# ---- build stage ----
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY go.mod ./
RUN go mod download || true
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/dnsforwarderd ./cmd/dnsforwarderd

# ---- runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache unbound tini bind-tools ca-certificates \
 && addgroup -S dnsfwd && adduser -S -G dnsfwd dnsfwd \
 && rm -f /etc/unbound/unbound.conf \
 && mkdir -p /config /etc/unbound /var/run \
 && chown -R dnsfwd:dnsfwd /config /etc/unbound /var/run

COPY --from=build /out/dnsforwarderd /usr/local/bin/dnsforwarderd

USER dnsfwd
EXPOSE 53/udp 53/tcp 80

ENV DNSFWD_CONFIG=/config/dnsforwarder.yaml \
    DNSFWD_OUT=/etc/unbound

# CAP_NET_BIND_SERVICE: bind port 53 without root
# CAP_SETUID/CAP_SETGID: allow unbound to drop privileges to dnsfwd
#                         (else it logs "unable to initgroups" and UDP can fail)
HEALTHCHECK --interval=30s --timeout=3s --retries=3 \
  CMD wget -qO- http://127.0.0.1:80/api/healthz || exit 1

ENTRYPOINT ["/sbin/tini", "--"]
CMD ["/usr/local/bin/dnsforwarderd", "-config", "/config/dnsforwarder.yaml", "-out", "/etc/unbound", "-unbound-bin", "/usr/sbin/unbound", "-conf-name", "dnsforwarder.conf"]
