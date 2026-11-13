#!/bin/sh
set -eu

BASE_URL=${BASE_URL:-http://localhost:8080}
USER_ID=20000000-0000-4000-8000-000000000001
TENANT_ID=10000000-0000-4000-8000-000000000001
mkdir -p artifacts
TOKEN=$(JWT_HMAC_SECRET=${JWT_HMAC_SECRET:-local-development-signing-secret-not-for-production} go run ./cmd/exportctl token "$USER_ID" "$TENANT_ID")
RESPONSE=$(curl -fsS -X POST "$BASE_URL/api/v1/exports" -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' -H "Idempotency-Key: demo-$(date +%s)" -d '{"format":"zip","datasets":["all"]}')
ID=$(printf '%s' "$RESPONSE" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
printf 'Export requested: %s\n' "$ID"
attempt=0
while [ "$attempt" -lt 180 ]; do
  STATUS=$(curl -fsS "$BASE_URL/api/v1/exports/$ID" -H "Authorization: Bearer $TOKEN")
  STATE=$(printf '%s' "$STATUS" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
  if [ "$STATE" = available ]; then break; fi
  if [ "$STATE" = failed ] || [ "$STATE" = canceled ]; then printf '%s\n' "$STATUS"; exit 1; fi
  attempt=$((attempt+1)); sleep 1
done
if [ "$STATE" != available ]; then echo 'Export did not finish within 180 seconds' >&2; exit 1; fi
curl -fsSL "$BASE_URL/api/v1/exports/$ID/download" -H "Authorization: Bearer $TOKEN" -o "artifacts/$ID.zip"
go run ./cmd/exportctl verify "artifacts/$ID.zip"
printf 'Export available and verified: artifacts/%s.zip\n' "$ID"
