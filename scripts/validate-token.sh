#!/usr/bin/env bash
#
# Validates that GitHub PATs have the permissions required by deploy-bot.
#
# Usage:
#   export GITHUB_ORG=your-org
#   export GITHUB_REPO=your-gitops-repo
#   export DEPLOY_BOT_TOKEN=github_pat_...
#   export DEPLOY_BOT_SCANNER_TOKEN=github_pat_...  # optional
#   ./scripts/validate-token.sh

set -euo pipefail

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[0;33m'
NC='\033[0m'

pass() { echo -e "  ${GREEN}✓${NC} $1"; }
fail() { echo -e "  ${RED}✗${NC} $1"; ERRORS=$((ERRORS + 1)); }
warn() { echo -e "  ${YELLOW}!${NC} $1"; }

check_api() {
  local token="$1" method="$2" url="$3"
  local status
  status=$(curl -s -o /dev/null -w "%{http_code}" \
    -X "$method" \
    -H "Authorization: Bearer ${token}" \
    -H "Accept: application/vnd.github+json" \
    "https://api.github.com${url}")
  echo "$status"
}

# --- Pre-flight ---

: "${GITHUB_ORG:?Set GITHUB_ORG to your GitHub organization}"
: "${GITHUB_REPO:?Set GITHUB_REPO to your gitops repository name}"
: "${DEPLOY_BOT_TOKEN:?Set DEPLOY_BOT_TOKEN to the primary bot PAT}"

ERRORS=0

# --- Primary token ---

echo ""
echo "Validating primary token (DEPLOY_BOT_TOKEN)"
echo "─────────────────────────────────────────────"

# Basic auth
status=$(check_api "$DEPLOY_BOT_TOKEN" GET "/user")
if [ "$status" = "200" ]; then
  pass "Token is valid"
else
  fail "Token authentication failed (HTTP ${status})"
  echo -e "\n${RED}Cannot continue — fix the token and re-run.${NC}"
  exit 1
fi

# Repo access
status=$(check_api "$DEPLOY_BOT_TOKEN" GET "/repos/${GITHUB_ORG}/${GITHUB_REPO}")
if [ "$status" = "200" ]; then
  pass "Can access ${GITHUB_ORG}/${GITHUB_REPO}"
else
  fail "Cannot access ${GITHUB_ORG}/${GITHUB_REPO} (HTTP ${status}) — check repository scope"
fi

# Contents: read (get default branch)
status=$(check_api "$DEPLOY_BOT_TOKEN" GET "/repos/${GITHUB_ORG}/${GITHUB_REPO}/git/ref/heads/main")
if [ "$status" = "200" ]; then
  pass "Contents: read (git refs)"
else
  # Try 'master' as fallback
  status=$(check_api "$DEPLOY_BOT_TOKEN" GET "/repos/${GITHUB_ORG}/${GITHUB_REPO}/git/ref/heads/master")
  if [ "$status" = "200" ]; then
    pass "Contents: read (git refs)"
  else
    fail "Contents: read — cannot read git refs (HTTP ${status})"
  fi
fi

# Pull requests: read
status=$(check_api "$DEPLOY_BOT_TOKEN" GET "/repos/${GITHUB_ORG}/${GITHUB_REPO}/pulls?state=closed&per_page=1")
if [ "$status" = "200" ]; then
  pass "Pull requests: read"
else
  fail "Pull requests: read (HTTP ${status})"
fi

# Issues: read (labels, comments)
status=$(check_api "$DEPLOY_BOT_TOKEN" GET "/repos/${GITHUB_ORG}/${GITHUB_REPO}/labels?per_page=1")
if [ "$status" = "200" ]; then
  pass "Issues: read (labels)"
else
  fail "Issues: read (HTTP ${status})"
fi

# Commit statuses: read
status=$(check_api "$DEPLOY_BOT_TOKEN" GET "/repos/${GITHUB_ORG}/${GITHUB_REPO}/commits/HEAD/status")
if [ "$status" = "200" ]; then
  pass "Commit statuses: read"
else
  warn "Commit statuses: read (HTTP ${status}) — may not be required"
fi

# Organization members: read (team membership)
status=$(check_api "$DEPLOY_BOT_TOKEN" GET "/orgs/${GITHUB_ORG}/teams?per_page=1")
if [ "$status" = "200" ]; then
  pass "Organization members: read (team listing)"
else
  fail "Organization members: read (HTTP ${status}) — required for team membership checks"
fi

# --- Write permission checks (non-destructive) ---
# We check that the /pulls endpoint accepts POST by looking at the status code.
# A 422 means we have write access but the payload is invalid (expected).
# A 403/404 means no write access.

status=$(check_api "$DEPLOY_BOT_TOKEN" POST "/repos/${GITHUB_ORG}/${GITHUB_REPO}/pulls")
if [ "$status" = "422" ]; then
  pass "Pull requests: write (can create PRs)"
elif [ "$status" = "403" ] || [ "$status" = "404" ]; then
  fail "Pull requests: write — no write access (HTTP ${status})"
else
  warn "Pull requests: write — unexpected response (HTTP ${status})"
fi

# Contents: write — check by attempting to create a ref (will 422 on bad payload)
status=$(check_api "$DEPLOY_BOT_TOKEN" POST "/repos/${GITHUB_ORG}/${GITHUB_REPO}/git/refs")
if [ "$status" = "422" ]; then
  pass "Contents: write (can create branches)"
elif [ "$status" = "403" ] || [ "$status" = "404" ]; then
  fail "Contents: write — no write access (HTTP ${status})"
else
  warn "Contents: write — unexpected response (HTTP ${status})"
fi

# Issues: write — check by attempting to create a label (will 422 on bad payload)
status=$(check_api "$DEPLOY_BOT_TOKEN" POST "/repos/${GITHUB_ORG}/${GITHUB_REPO}/labels")
if [ "$status" = "422" ]; then
  pass "Issues: write (can manage labels)"
elif [ "$status" = "403" ] || [ "$status" = "404" ]; then
  fail "Issues: write — no write access (HTTP ${status})"
else
  warn "Issues: write — unexpected response (HTTP ${status})"
fi

# --- Scanner token (optional) ---

if [ -n "${DEPLOY_BOT_SCANNER_TOKEN:-}" ]; then
  echo ""
  echo "Validating scanner token (DEPLOY_BOT_SCANNER_TOKEN)"
  echo "─────────────────────────────────────────────────────"

  status=$(check_api "$DEPLOY_BOT_SCANNER_TOKEN" GET "/user")
  if [ "$status" = "200" ]; then
    pass "Token is valid"
  else
    fail "Token authentication failed (HTTP ${status})"
  fi

  # Can list org repos
  status=$(check_api "$DEPLOY_BOT_SCANNER_TOKEN" GET "/orgs/${GITHUB_ORG}/repos?per_page=1")
  if [ "$status" = "200" ]; then
    pass "Can list organization repos"
  else
    fail "Cannot list organization repos (HTTP ${status}) — check repository scope"
  fi

  # Contents: read — verify we can read file contents from a repo
  status=$(check_api "$DEPLOY_BOT_SCANNER_TOKEN" GET "/repos/${GITHUB_ORG}/${GITHUB_REPO}/contents/README.md")
  if [ "$status" = "200" ]; then
    pass "Contents: read (can read repo files)"
  elif [ "$status" = "404" ]; then
    # 404 might mean the file doesn't exist but we have access — try the repo root
    status=$(check_api "$DEPLOY_BOT_SCANNER_TOKEN" GET "/repos/${GITHUB_ORG}/${GITHUB_REPO}/contents/")
    if [ "$status" = "200" ]; then
      pass "Contents: read (can read repo files)"
    else
      fail "Contents: read (HTTP ${status})"
    fi
  else
    fail "Contents: read (HTTP ${status})"
  fi

  # Commit statuses: write — check by attempting to create a status (will 422 on bad payload)
  status=$(check_api "$DEPLOY_BOT_SCANNER_TOKEN" POST "/repos/${GITHUB_ORG}/${GITHUB_REPO}/statuses/0000000000000000000000000000000000000000")
  if [ "$status" = "422" ]; then
    pass "Commit statuses: write (can set validation status)"
  elif [ "$status" = "403" ] || [ "$status" = "404" ]; then
    fail "Commit statuses: write — no write access (HTTP ${status})"
  else
    warn "Commit statuses: write — unexpected response (HTTP ${status})"
  fi
else
  echo ""
  echo "Scanner token (DEPLOY_BOT_SCANNER_TOKEN)"
  echo "─────────────────────────────────────────"
  warn "Not set — skipping. The primary token will be used for scanning."
fi

# --- Summary ---

echo ""
if [ "$ERRORS" -eq 0 ]; then
  echo -e "${GREEN}All checks passed.${NC}"
else
  echo -e "${RED}${ERRORS} check(s) failed.${NC} Review the errors above and update the token permissions."
  exit 1
fi
