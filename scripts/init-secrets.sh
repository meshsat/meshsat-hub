#!/bin/sh
# init-secrets.sh — Auto-generates service passwords on first boot.
#
# Called by the secrets-init Docker service before any other service starts.
# Creates /data/secrets/.secrets.env with random passwords for NATS, Redis,
# and leafnode. Subsequent boots skip generation (passwords persist).
#
# The .secrets.env file is mounted as env_file in docker-compose, so all
# services read the passwords automatically. No manual env var setup needed.

SECRETS_DIR="${SECRETS_DIR:-/data/secrets}"
SECRETS_FILE="${SECRETS_DIR}/.secrets.env"

mkdir -p "$SECRETS_DIR"

# Skip if secrets already exist (not first boot).
if [ -f "$SECRETS_FILE" ]; then
  echo "init-secrets: secrets already exist at $SECRETS_FILE, skipping generation"
  exit 0
fi

echo "init-secrets: first boot — generating service passwords..."

# Generate random 32-byte hex passwords.
NATS_MQTT_PASSWORD=$(head -c 32 /dev/urandom | od -A n -t x1 | tr -d ' \n')
NATS_LEAFNODE_TOKEN=$(head -c 32 /dev/urandom | od -A n -t x1 | tr -d ' \n')
REDIS_PASSWORD=$(head -c 32 /dev/urandom | od -A n -t x1 | tr -d ' \n')

cat > "$SECRETS_FILE" << EOF
# Auto-generated service passwords ($(date -u +%Y-%m-%dT%H:%M:%SZ))
# Do NOT edit manually — use Hub Settings > Security to rotate.
NATS_MQTT_PASSWORD=${NATS_MQTT_PASSWORD}
NATS_LEAFNODE_TOKEN=${NATS_LEAFNODE_TOKEN}
REDIS_PASSWORD=${REDIS_PASSWORD}

# Constructed URLs with embedded credentials (used by Hub service).
HUB_MQTT_BROKER_URL=tcp://meshsat:${NATS_MQTT_PASSWORD}@nats:1883
HUB_NATS_URL=tcp://meshsat:${NATS_MQTT_PASSWORD}@nats:1883
HUB_REDIS_URL=redis://:${REDIS_PASSWORD}@redis:6379/0
EOF

chmod 600 "$SECRETS_FILE"
echo "init-secrets: passwords written to $SECRETS_FILE"
echo "init-secrets: NATS MQTT auth, leafnode token, and Redis password configured"
