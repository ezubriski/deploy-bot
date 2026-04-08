REGISTRY ?= ## Set to your ECR registry, e.g. 123456789012.dkr.ecr.us-west-2.amazonaws.com
IMAGE    := $(REGISTRY)/deploy-bot
TAG      ?= $(shell git rev-parse --short HEAD)
REGION   ?= us-west-2

# Integration test settings
ENV_FILE        ?= .env.integration
INTEG_TIMEOUT   ?= 10m
BUMP            ?= patch
INTEG_RUN       ?=  # empty = run all; set to -run TestFoo to filter

.DEFAULT_GOAL := build

.PHONY: build build-linux bot receiver deploy-bot-config test test-unit test-pkg test-integ test-integ-single \
        fmt fmt-check lint check image push ecr-login release \
        docs-setup docs-serve docs-build docs-deploy clean help

# --- build ---

build: bot receiver deploy-bot-config ## Build all binaries to ./bin

build-linux: ## Build all binaries for linux/amd64 to ./bin (used by image)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(MAKE) build

bot: ## Build cmd/bot -> bin/bot
	go build -trimpath -o bin/bot ./cmd/bot

receiver: ## Build cmd/receiver -> bin/receiver
	go build -trimpath -o bin/receiver ./cmd/receiver

deploy-bot-config: ## Build cmd/deploy-bot-config -> bin/deploy-bot-config
	go build -trimpath -o bin/deploy-bot-config ./cmd/deploy-bot-config

# --- test ---

test: test-unit ## Run unit tests (alias for test-unit)

test-unit: ## Run all unit tests with race detector (no integration tag)
	go test -race ./...

test-pkg: ## Run tests for a single package: make test-pkg PKG=./internal/store/...
	go test $(PKG)

test-integ: ## Run all integration tests (loads $(ENV_FILE))
	set -a && . ./$(ENV_FILE) && set +a && \
	go test -tags=integration -v -count=1 -timeout=$(INTEG_TIMEOUT) ./tests/integration/...

test-integ-single: ## Run one integration test: make test-integ-single RUN=TestDeployAndApprove
	set -a && . ./$(ENV_FILE) && set +a && \
	go test -tags=integration -v -count=1 -timeout=$(INTEG_TIMEOUT) -run $(RUN) ./tests/integration/...

# --- format & lint ---

fmt: ## Run gofmt on all Go files (writes changes)
	gofmt -w .

fmt-check: ## Check gofmt (fails if any file needs formatting)
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

lint: ## Run golangci-lint
	golangci-lint run

check: fmt-check lint test-unit ## Run fmt check, lint, and unit tests

# --- image ---

image: build-linux ## Build container image with Podman (TAG defaults to git short SHA)
	podman build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

push: image ## Build and push image to ECR
	podman push $(IMAGE):$(TAG)
	podman push $(IMAGE):latest

ecr-login: ## Authenticate Podman to ECR
	aws ecr get-login-password --region $(REGION) | \
		podman login --username AWS --password-stdin $(REGISTRY)

# --- release ---

release: ## Trigger release workflow: make release BUMP=minor
	gh workflow run release.yml -f bump=$(BUMP)
	@echo "Release workflow triggered with bump=$(BUMP)"

# --- docs ---

VENV := .venv
MKDOCS := $(VENV)/bin/mkdocs
MIKE := $(VENV)/bin/mike

docs-setup: ## Create venv and install doc dependencies
	python3 -m venv $(VENV)
	$(VENV)/bin/pip install -r requirements-docs.txt

docs-serve: $(MKDOCS) ## Serve docs locally at http://127.0.0.1:8000
	$(MKDOCS) serve

docs-build: $(MKDOCS) ## Build static docs site to ./site
	$(MKDOCS) build --strict

docs-deploy: $(MIKE) ## Deploy docs to gh-pages (local): make docs-deploy VERSION=dev
	$(MIKE) deploy --update-aliases $(VERSION) latest

$(MKDOCS) $(MIKE):
	$(MAKE) docs-setup

# --- misc ---

clean: ## Remove build artifacts
	rm -rf bin/ site/

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'
