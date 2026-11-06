#!/bin/sh
# Package post-install: create the unprivileged user and writable directories.
# It deliberately does not start or enable the service; the operator decides.
set -e

if ! getent group lgllm >/dev/null 2>&1; then
    if command -v addgroup >/dev/null 2>&1; then
        addgroup -S lgllm 2>/dev/null || true
    else
        groupadd -r lgllm 2>/dev/null || true
    fi
fi

if ! getent passwd lgllm >/dev/null 2>&1; then
    if command -v adduser >/dev/null 2>&1; then
        adduser -S -D -H -s /sbin/nologin -G lgllm lgllm 2>/dev/null || true
    else
        useradd -r -g lgllm -s /sbin/nologin -M lgllm 2>/dev/null || true
    fi
fi

install -d -m 0750 -o lgllm -g lgllm /var/lib/lgllm 2>/dev/null || true
install -d -m 0750 /etc/lgllm 2>/dev/null || true

cat <<'EOF'
lgllm installed.

Next steps:
  1. cp /etc/lgllm/config.example.yaml /etc/lgllm/config.yaml
  2. edit providers/connections (or set ${ENV} references in /etc/lgllm/env)
  3. /usr/local/bin/lgllm -config /etc/lgllm/config.yaml -check
  4. systemctl enable --now lgllm

If you enable the admin API, point admin.state_file at /var/lib/lgllm/admin.yaml.
EOF
exit 0