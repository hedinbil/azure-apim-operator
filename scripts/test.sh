#!/usr/bin/env bash
# set -euo pipefail

# Default API endpoint
# Replace with your API endpoint for testing
API_URL="${API_URL:-https://api.example.com/v3/user}"

# API Management subscription key (Ocp-Apim-Subscription-Key)
# IMPORTANT: Set this via environment variable or replace with your test key
# This key should be revoked if it was previously committed to version control
SUBSCRIPTION_KEY="${SUBSCRIPTION_KEY:-your-subscription-key-here}"

# JSON payload
read -r -d '' PAYLOAD <<'EOF'
{
  "id": 123,
  "username": "jdoe",
  "firstName": "John",
  "lastName": "Doe",
  "email": "jdoe@example.com",
  "password": "s3cr3t!",
  "phone": "555-1234",
  "userStatus": 1
}
EOF

# Perform CURL request, capture response body and status code
response=$(curl -s -w "\n%{http_code}" -X POST "$API_URL" \
  -H "Content-Type: application/json" \
  -H "Accept: application/json" \
  -H "Ocp-Apim-Subscription-Key: $SUBSCRIPTION_KEY" \
  -d "$PAYLOAD")

# Extract HTTP status code (last line) and response body (all but last line)
http_code=$(echo "$response" | tail -n1)
response_body=$(echo "$response" | sed '$d')

# Display the HTTP status and response body
echo "HTTP Status: $http_code"
echo "Response Body: $response_body"