# Contributing

## Getting started

1. Fork the repository and clone your fork
2. Create a branch: `git checkout -b your-feature`
3. Make your changes
4. Run tests: `make test`
5. Push and open a pull request against `main`

## Development

```bash
make build    # compile both binaries to ./bin
make test     # unit tests (no external dependencies)
make lint     # golangci-lint
```

Unit tests use `miniredis` — no real Redis, AWS, Slack, or GitHub credentials needed.

## Integration tests

Integration tests run against live external services and are not expected to pass in forks. They require a `.env.integration` file with credentials for a real Slack workspace, GitHub org, Redis instance, and AWS account. See the README for the full list of required environment variables.

## Pull requests

- Keep PRs focused — one logical change per PR
- All unit tests must pass (`make test`)
- Match the existing code style; run `make lint` before opening
- Update `CLAUDE.md` if you change commands, architecture, or package responsibilities
- Integration tests are run manually by maintainers before merging

## Issues

Bug reports and feature requests are welcome. Please search existing issues before opening a new one.
