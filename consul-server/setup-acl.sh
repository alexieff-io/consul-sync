#!/bin/bash
# Run this after first starting Consul to bootstrap ACL and create tokens.
# Requires: curl, jq
set -euo pipefail

CONSUL_ADDR="${CONSUL_ADDR:-http://127.0.0.1:8500}"

echo "==> Bootstrapping ACL system..."
BOOTSTRAP=$(curl -s -X PUT "${CONSUL_ADDR}/v1/acl/bootstrap")
MASTER_TOKEN=$(echo "$BOOTSTRAP" | jq -r '.SecretID')
echo "    Master token: ${MASTER_TOKEN}"
echo "    SAVE THIS TOKEN SECURELY!"

echo ""
echo "==> Creating read-only policy for consul-sync..."
curl -s -X PUT "${CONSUL_ADDR}/v1/acl/policy" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d '{
    "Name": "consul-sync-read",
    "Description": "Read-only access for consul-sync controller",
    "Rules": "service_prefix \"\" { policy = \"read\" } node_prefix \"\" { policy = \"read\" }"
  }' | jq .

echo ""
echo "==> Creating token for consul-sync..."
SYNC_TOKEN=$(curl -s -X PUT "${CONSUL_ADDR}/v1/acl/token" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d '{
    "Description": "consul-sync controller",
    "Policies": [{"Name": "consul-sync-read"}]
  }' | jq -r '.SecretID')
echo "    consul-sync token: ${SYNC_TOKEN}"
echo "    Store this in 1Password as 'consul-acl-token'"

echo ""
echo "==> Creating write policy for service registration..."
curl -s -X PUT "${CONSUL_ADDR}/v1/acl/policy" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d '{
    "Name": "service-registration",
    "Description": "Register and deregister services",
    "Rules": "service_prefix \"\" { policy = \"write\" } node_prefix \"\" { policy = \"write\" }"
  }' | jq .

echo ""
echo "==> Creating token for service registration (agent)..."
AGENT_TOKEN=$(curl -s -X PUT "${CONSUL_ADDR}/v1/acl/token" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d '{
    "Description": "Consul agent and service registration",
    "Policies": [{"Name": "service-registration"}]
  }' | jq -r '.SecretID')
echo "    Agent token: ${AGENT_TOKEN}"

echo ""
echo "==> Setting agent token..."
curl -s -X PUT "${CONSUL_ADDR}/v1/agent/token/agent" \
  -H "X-Consul-Token: ${MASTER_TOKEN}" \
  -d "{\"Token\": \"${AGENT_TOKEN}\"}" | jq .

echo ""
echo "==> Done! Summary:"
echo "    Master token:       ${MASTER_TOKEN}"
echo "    consul-sync token:  ${SYNC_TOKEN} (store in 1Password as 'consul-acl-token')"
echo "    Agent token:        ${AGENT_TOKEN}"
