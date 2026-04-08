# deploy-bot

A Slack bot that gates Kubernetes deployments behind an approval workflow. Developers request deployments via `/deploy` or `@bot deploy`, approvers approve or reject with Slack buttons, and the bot creates and merges GitHub PRs that update kustomize image tags in a GitOps repo. Argo CD picks up merged PRs and deploys.

Built for organizations running Kubernetes + Argo CD that want centralized, auditable deployment control without leaving Slack.

## Why deploy-bot

- **No public network exposure.** Socket Mode (outbound WebSocket) and SQS long-polling. No ingress, no webhooks, no load balancer.
- **ECR push-triggered deploys.** One EventBridge rule captures all ECR pushes account-wide. The bot filters by app and tag pattern. Add a new app and it works immediately -- no EventBridge changes, no GitHub webhooks, no per-repo CI pipelines.
- **Batteries included.** Terraform module, Kustomize base, Slack app manifest, GitHub Action and CLI for config validation.
- **Simple app configuration.** Define apps in `config.json` and the bot picks them up on the next hot-reload. For self-service, optional [repo-sourced discovery](repo-sourced-app-discovery.md) lets app teams drop a `.deploy-bot.json` in their repo.
- **Convention over configuration.** With [enforced naming conventions](naming-conventions.md), app names and kustomize paths are derived from repository names. Onboarding a new app takes two lines of JSON.
- **Built for resilience.** Redis Streams consumer groups, in-memory buffer with backpressure, sweeper for expired deploys, automatic rebase on merge conflicts, GitHub reconciliation after data loss.
- **OpenTelemetry instrumented.** GitHub, Slack, AWS, and Redis I/O is observed via OTEL contrib libraries; metrics export to Prometheus by default, with standard OTEL env vars for routing to a collector. See [observability](observability.md).
- **Horizontal scaling.** Receiver and worker scale independently. Consumer groups ensure each event processes once.

## Architecture

```
Developer          Receiver          Redis Stream       Worker            GitHub / Argo CD
    |                  |                   |               |                     |
    |-- /deploy ------>|                   |               |                     |
    |   @bot deploy    |-- enqueue ------->|               |                     |
    |                  |<-- ack -----------|               |                     |
    |                  |                   |-- event ----->|                     |
    |                  |                   |               |-- create PR ------->|
    |                  |                   |               |                     |
Approver               |                   |               |                     |
    |-- Approve ------>|                   |               |                     |
    |                  |-- enqueue ------->|               |                     |
    |                  |                   |-- event ----->|                     |
    |                  |                   |               |-- merge PR -------->|
    |                  |                   |               |                     |-- deploy
    |                  |                   |               |                     |
ECR Push               |                   |               |                     |
    |  EventBridge --->|                   |               |                     |
    |  (SQS)           |-- enqueue ------->| ecr:events    |                     |
    |                  |                   |-- event ----->|                     |
    |                  |                   |               |-- create PR ------->|
    |                  |                   |               |   (auto-merge or    |
    |                  |                   |               |    request approval)|
```

Two processes share a single container image:

- **receiver** -- connects to Slack via Socket Mode, validates incoming events, and enqueues them to a Redis Stream. Also polls SQS for ECR push events and scans repos for app config. Stateless; run 2+ replicas.
- **worker** -- consumes events from both streams, prioritizing user events. Runs all business logic (GitHub API, ECR, audit logging). Run 2+ replicas; Redis Streams consumer groups ensure each event is processed once.

## Getting started

| Guide | Time | What you get |
|---|---|---|
| **[Quickstart](quickstart.md)** | ~30 min | IRSA roles, in-cluster Redis, no ECR events. Kick the tires. |
| **[Production setup](production-setup.md)** | ~1 hour | ElastiCache IAM auth, WORM audit bucket, CMK encryption, ECR push deploys, repo discovery. |

## Commands

### Slash commands

| Command | Description |
|---|---|
| `/deploy` | Open the deployment request modal |
| `/deploy <app-env>` | Open the modal pre-selected to an app |
| `/deploy list` | List all pending deployments (alias: `status`) |
| `/deploy history [app-env]` | Show recent completed deployments |
| `/deploy apps` | List all configured apps and their source |
| `/deploy conflicts` | List repo-sourced apps blocked by operator config |
| `/deploy tags <app-env>` | List the 20 most recent valid tags |
| `/deploy tags <app-env> <tag>` | Verify a specific tag exists in ECR |
| `/deploy cancel <pr>` | Cancel your own pending deployment |
| `/deploy nudge <pr>` | Re-ping the approver |
| `/deploy rollback <app-env>` | Re-deploy the previous approved tag |
| `/deploy help` | Show command help |

### @mention commands

All commands are available via `@bot <command>` in any channel. Responses are posted in-channel (threaded if in a thread).

| Command | Description |
|---|---|
| `@bot deploy <app-env> <tag> [@approver] [reason]` | Create a deploy PR with positional args |
| `@bot list` | List pending deployments |
| `@bot history [app-env]` | Show recent deploys |
| `@bot apps` | List configured apps |
| `@bot conflicts` | List config conflicts |
| `@bot tags <app-env>` | List recent tags |
| `@bot cancel <pr>` | Cancel your own deployment |
| `@bot nudge <pr>` | Remind the approver |
| `@bot rollback <app-env>` | Deploy the previous tag |
| `@bot help` | Show command help |
