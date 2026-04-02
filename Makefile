REGISTRY := 123456789012.dkr.ecr.us-west-2.amazonaws.com
IMAGE    := $(REGISTRY)/deploy-bot
TAG      ?= $(shell git rev-parse --short HEAD)
REGION   := us-west-2

# Integration test settings
ENV_FILE        ?= .env.integration
INTEG_TIMEOUT   ?= 10m
INTEG_RUN       ?=  # empty = run all; set to -run TestFoo to filter

.DEFAULT_GOAL := build

.PHONY: build bot receiver test test-unit test-pkg test-integ test-integ-single \
        lint image push ecr-login clean help

# --- build ---

build: bot receiver ## Build both binaries to ./bin

bot: ## Build cmd/bot -> bin/bot
	go build -trimpath -o bin/bot ./cmd/bot

receiver: ## Build cmd/receiver -> bin/receiver
	go build -trimpath -o bin/receiver ./cmd/receiver

# --- test ---

test: test-unit ## Run unit tests (alias for test-unit)

test-unit: ## Run all unit tests (no integration tag)
	go test ./...

test-pkg: ## Run tests for a single package: make test-pkg PKG=./internal/store/...
	go test $(PKG)

test-integ: ## Run all integration tests (loads $(ENV_FILE))
	set -a && . $(ENV_FILE) && set +a && \
	go test -tags=integration -v -count=1 -timeout=$(INTEG_TIMEOUT) ./tests/integration/...

test-integ-single: ## Run one integration test: make test-integ-single RUN=TestDeployAndApprove
	set -a && . $(ENV_FILE) && set +a && \
	go test -tags=integration -v -count=1 -timeout=$(INTEG_TIMEOUT) -run $(RUN) ./tests/integration/...

# --- lint ---

lint: ## Run golangci-lint
	golangci-lint run

# --- image ---

image: ## Build container image with Podman (TAG defaults to git short SHA)
	podman build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

push: image ## Build and push image to ECR
	podman push $(IMAGE):$(TAG)
	podman push $(IMAGE):latest

ecr-login: ## Authenticate Podman to ECR
	aws ecr get-login-password --region $(REGION) | \
		podman login --username AWS --password-stdin $(REGISTRY)

# --- misc ---

clean: ## Remove build artifacts
	rm -rf bin/

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-20s %s\n", $$1, $$2}'
