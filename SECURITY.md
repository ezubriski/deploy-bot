# Security Policy

## Reporting a vulnerability

Please do not open a public GitHub issue for security vulnerabilities.

Report them privately using [GitHub's private vulnerability reporting](../../security/advisories/new). This keeps the details confidential until a fix is available.

Include as much detail as you can: what the vulnerability is, how to reproduce it, and what impact it could have.

## Scope

This project is a self-hosted Slack bot. The attack surface is:

- The bot process and receiver process running in your Kubernetes cluster
- The Redis instance used for state
- The GitHub token used to create and merge PRs
- The Slack tokens used to receive and send messages

Secrets are expected to be stored in AWS Secrets Manager, not in config files or environment variables directly.
