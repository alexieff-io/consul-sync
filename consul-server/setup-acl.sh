#!/bin/bash
# Run this after first starting Consul to bootstrap ACL and create tokens.
# Requires: curl, jq
set -euo pipefail

CONSUL_ADDR="${CONSUL_ADDR:-http://127.0.0.1:8500}"

# Wait for Consul to be ready
echo "==> Waiting for Consul to be ready..."
until curl -sf "${CONSUL_ADDR}/v1/status/leader" > /dev/null 2>&1; do
  echo "    Consul not ready, retrying in 2s..."
  sleep 2
done
echo "    Consul is ready."

echo ""
echo "==> Bootstrapping ACL system..."
BOOTSTRAP=$(curl -sf -X PUT "${CONSUL_ADDR}/v1/acl/bootstrap") || {
  echo "    ERROR: ACL bootstrap failed. Response:"
  echo "    $(curl -s -X PUT "${CONSUL_ADDR}/v1/acl/bootstrap")"
  echo "    If ACLs were already bootstrapped, provide the master token:"
  read -rp "    Master token: " MASTER_TOKEN
  if [ -z "$MASTER_TOKEN" ]; then
    echo "    No token provided, exiting."
    exit 1
  fi
}

if [ -z "${MASTER_TOKEN:-}" ]; then
  MASTER_TOKEN=$(echo "$BOOTSTRAP" | jq -r '.SecretID')
  echo "    Master token: ${MASTER_TOKEN}"
  echo "    SAVE THIS TOKEN SECURELY!"
fi

echo ""
echo "==> Creating read-only policy for consul-sync..."
RESULT=$(curl -sf -X PUT "${CONSUL_ADDR}/v1/acl/policy" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d '{
    "Name": "consul-sync-read",
    "Description": "Read-only access for consul-sync controller",
    "Rules": "service_prefix \"\" { policy = \"read\" } node_prefix \"\" { policy = \"read\" }"
  }') && echo "$RESULT" | jq . || echo "    Policy may already exist, continuing..."

echo ""
echo "==> Creating token for consul-sync..."
SYNC_TOKEN=$(curl -sf -X PUT "${CONSUL_ADDR}/v1/acl/token" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d '{
    "Description": "consul-sync controller",
    "Policies": [{"Name": "consul-sync-read"}]
  }' | jq -r '.SecretID')
echo "    consul-sync token: ${SYNC_TOKEN}"
echo "    Store this in 1Password as 'consul-acl-token'"

echo ""
echo "==> Creating write policy for service registration..."
RESULT=$(curl -sf -X PUT "${CONSUL_ADDR}/v1/acl/policy" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d '{
    "Name": "service-registration",
    "Description": "Register and deregister services",
    "Rules": "service_prefix \"\" { policy = \"write\" } node_prefix \"\" { policy = \"write\" }"
  }') && echo "$RESULT" | jq . || echo "    Policy may already exist, continuing..."

echo ""
echo "==> Creating token for service registration (agent)..."
AGENT_TOKEN=$(curl -sf -X PUT "${CONSUL_ADDR}/v1/acl/token" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d '{
    "Description": "Consul agent and service registration",
    "Policies": [{"Name": "service-registration"}]
  }' | jq -r '.SecretID')
echo "    Agent token: ${AGENT_TOKEN}"

echo ""
echo "==> Setting agent token..."
curl -sf -X PUT "${CONSUL_ADDR}/v1/agent/token/agent" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d "{\"Token\": \"${AGENT_TOKEN}\"}" | jq .

echo ""
echo "==> Done! Summary:"
echo "    Master token:       ${MASTER_TOKEN}"
echo "    consul-sync token:  ${SYNC_TOKEN} (store in 1Password as 'consul-acl-token')"
echo "    Agent token:        ${AGENT_TOKEN}"
