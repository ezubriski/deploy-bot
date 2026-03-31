#!/usr/bin/env bash
# Run integration tests. Uses your ambient AWS credentials to fetch the
# dedicated integ-test credentials from Secrets Manager, then runs the
# test suite as that identity.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
ENV_FILE="${REPO_ROOT}/.env.integration"

if [[ ! -f "${ENV_FILE}" ]]; then
  echo "error: ${ENV_FILE} not found" >&2
  exit 1
fi

set -a
source "${ENV_FILE}"
set +a

# Fetch integ AWS credentials from Secrets Manager using ambient creds.
secret_json=$(aws secretsmanager get-secret-value \
  --secret-id "${INTEG_AWS_SECRET}" \
  --region "${AWS_REGION}" \
  --query SecretString --output text)

export AWS_ACCESS_KEY_ID
export AWS_SECRET_ACCESS_KEY
AWS_ACCESS_KEY_ID=$(echo "${secret_json}" | python3 -c "import json,sys; print(json.load(sys.stdin)['aws_access_key_id'])")
AWS_SECRET_ACCESS_KEY=$(echo "${secret_json}" | python3 -c "import json,sys; print(json.load(sys.stdin)['aws_secret_access_key'])")

exec go test -count=1 -tags=integration -timeout=5m "$@" "${REPO_ROOT}/tests/integration/"
