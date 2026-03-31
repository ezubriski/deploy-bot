# Integration test environment setup

Integration tests run the worker end-to-end against real services. Events are
injected directly into the Redis stream (bypassing the receiver and Socket Mode),
the worker processes them with real GitHub and Slack API calls, and tests assert
against GitHub state and Redis state.

---

## What you need

| Service | Purpose | Stub? |
|---|---|---|
| Redis | Event stream and state | Local Docker |
| GitHub | PR create/merge/close, labels, team membership | Real test repo |
| Slack | Outbound messages (postMessage, DMs) | Real test workspace |
| AWS Secrets Manager | Bot secrets at startup | Real test account |
| AWS ECR | Image tag validation | Real test account |
| AWS S3 | Audit log writes | Real test account |

---

## 1. GitHub

### Test repository

Create a repository for the bot to commit to (e.g. `<your-org>/deploy-bot-test-gitops`).

Add a kustomization file at the path you will configure in `config.json`:

```yaml
# apps/test-app/kustomization.yaml
resources:
  - deployment.yaml
images:
  - name: test-app
    newTag: v0.0.0
```

Enable **Automatically delete head branches** in the repository settings
(`Settings → General → Pull Requests`) so merged branches are cleaned up.

### Labels

Create both labels in the test repository:

```bash
gh label create "deploy-bot" --color "0075ca" --repo <your-org>/deploy-bot-test-gitops
gh label create "deploy-bot/pending" --color "0075ca" --repo <your-org>/deploy-bot-test-gitops
```

### Teams

Create a GitHub team (e.g. `deploy-bot-testers`) in your org. Both the
requester and approver test accounts must be members. Use this team for both
`deployer_team` and `approver_team` in the test config.

The team members must have Slack accounts whose **email address** matches
their GitHub account's primary email. The validator resolves
`Slack user ID → email → GitHub login → team membership`, so the email
link is required.

### Fine-grained PAT

Create a fine-grained PAT scoped to the `deploy-bot-test-gitops` repository.
Required permissions are identical to production — see `README.md`. Store the
token value; it goes into Secrets Manager below.

---

## 2. AWS

All resources should live in a dedicated test account or clearly isolated
namespace (e.g. prefix everything with `deploy-bot-test-`).

### ECR repository

```bash
aws ecr create-repository --repository-name deploy-bot-test-app
```

Push a test image with a tag that matches the configured `tag_pattern`. A
minimal scratch image is sufficient — the bot only validates that the tag exists,
it does not pull the image.

```bash
aws ecr get-login-password | docker login --username AWS --password-stdin \
  <account>.dkr.ecr.<region>.amazonaws.com

docker pull hello-world
docker tag hello-world <account>.dkr.ecr.<region>.amazonaws.com/deploy-bot-test-app:v0.0.1
docker push <account>.dkr.ecr.<region>.amazonaws.com/deploy-bot-test-app:v0.0.1
```

Repeat for any additional test tags you want to deploy.

### S3 bucket

```bash
aws s3api create-bucket --bucket deploy-bot-test-audit --region us-east-1
```

### IAM roles

Create three roles following the same pattern as production (`README.md` →
IAM section), substituting test resource ARNs. Name them something like:

- `deploy-bot-test` (bot role, assumes the two below)
- `deploy-bot-test-ecr`
- `deploy-bot-test-audit`

For local development the bot role is not needed — your local AWS credentials
(via `~/.aws/credentials` or environment variables) are used directly. The ECR
and audit roles are still assumed via STS, so your local credentials must have
`sts:AssumeRole` permission for those two roles.

### Secrets Manager

Create a secret at the path you will pass as `AWS_SECRET_NAME`:

```bash
aws secretsmanager create-secret \
  --name deploy-bot/test-secrets \
  --secret-string '{
    "slack_bot_token": "xoxb-your-test-bot-token",
    "slack_app_token": "xapp-your-test-app-token",
    "github_token":    "github_pat_your_test_token",
    "redis_addr":      "127.0.0.1:6379"
  }'
```

`redis_token` is omitted if your local Redis has no auth.

---

## 3. Slack

Create a **separate** Slack app for integration testing — do not reuse the
production app. Follow the same setup steps as in `README.md` (Socket Mode,
slash command, bot scopes). Socket Mode does not need to be connected for
integration tests — only the bot token and outbound API calls are used.

**Test channel**: Create a private channel (e.g. `#deploy-bot-integration-test`)
and invite the test bot. Note the channel ID (`C...`).

**Test users**: You need two Slack user IDs:
- `INTEGRATION_REQUESTER_ID` — the person requesting the deploy
- `INTEGRATION_APPROVER_ID` — the person approving it

These can be real team member accounts or a second bot/test user account,
as long as both are in the `deploy-bot-testers` GitHub team (via email match).

---

## 4. Redis

Run a local Redis instance:

```bash
docker run -d --name deploy-bot-test-redis -p 6379:6379 redis:alpine
```

The integration tests talk directly to this Redis. The tests do not flush the
entire database, but they do create and delete keys for the configured app.
Do not point at a shared Redis that other services use.

---

## 5. Test config file

Copy the example and fill in your values:

```bash
cp tests/integration/testdata/config.json.example tests/integration/testdata/config.json
```

`tests/integration/testdata/config.json` is gitignored. Never commit real
values.

---

## 6. Environment variables

| Variable | Description |
|---|---|
| `AWS_SECRET_NAME` | Secrets Manager path (e.g. `deploy-bot/test-secrets`) |
| `CONFIG_PATH` | Path to test config (default: `tests/integration/testdata/config.json`) |
| `INTEGRATION_APP` | App name to use in tests — must match an entry in `config.json` |
| `INTEGRATION_TAG` | ECR tag to deploy — must exist in the ECR repo (e.g. `v0.0.1`) |
| `INTEGRATION_REQUESTER_ID` | Slack user ID acting as the deploy requester |
| `INTEGRATION_APPROVER_ID` | Slack user ID acting as the approver (must be in approver team) |
| `AWS_PROFILE` or `AWS_REGION` | Standard AWS SDK env vars for credentials and region |

Set these in a `.env.integration` file (gitignored) and source it before running:

```bash
export AWS_SECRET_NAME=deploy-bot/test-secrets
export AWS_REGION=us-east-1
export INTEGRATION_APP=test-app
export INTEGRATION_TAG=v0.0.1
export INTEGRATION_REQUESTER_ID=U01234ABCDE
export INTEGRATION_APPROVER_ID=U09876ZYXWV
```

---

## 7. Running the tests

```bash
source .env.integration
go test -v -tags=integration ./tests/integration/ -timeout=300s
```

Run a single test:

```bash
go test -v -tags=integration ./tests/integration/ -run TestDeployAndApprove -timeout=120s
```

Tests are not safe to run in parallel (`-parallel=1` is enforced in the harness)
because they share the app's deploy lock and GitHub test repository.

---

## What the tests do and do not cover

**Covered by integration tests:**
- Deploy modal submission → GitHub PR creation with correct content and labels
- Approve action → PR merged, Redis state cleaned up, history entry pushed
- Reject submission → PR closed, Redis state cleaned up, history entry pushed
- Slack outbound messages (DMs to requester and approver, deploy channel post)
- Audit log S3 writes
- ECR tag validation (tag must actually exist in ECR)
- Approver team membership check via GitHub API

**Not covered (manual testing only):**
- Receiver Socket Mode connection and event ACK semantics
- In-memory buffer behaviour during Redis unavailability
- Modal rendering and UI interactions in Slack
