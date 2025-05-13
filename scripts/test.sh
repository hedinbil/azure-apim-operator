#!/usr/bin/env bash
# set -euo pipefail

# Default API endpoint
# API_URL="https://petstore3.operations-test.external.hedinit.io/v3/user"
API_URL="https://api-dev.hedinit.com/petstore3/user"

# API Management subscription key (Ocp-Apim-Subscription-Key)
SUBSCRIPTION_KEY="a41a90fea9314469a885e13fb3ac7023"

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