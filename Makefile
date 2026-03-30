REGISTRY := 123456789012.dkr.ecr.us-west-2.amazonaws.com
IMAGE    := $(REGISTRY)/deploy-bot
TAG      ?= $(shell git rev-parse --short HEAD)
REGION   := us-west-2

.DEFAULT_GOAL := build

.PHONY: build bot receiver test lint docker-build docker-push ecr-login clean help

build: bot receiver ## Build both binaries to ./bin

bot: ## Build cmd/bot -> bin/bot
	go build -trimpath -o bin/bot ./cmd/bot

receiver: ## Build cmd/receiver -> bin/receiver
	go build -trimpath -o bin/receiver ./cmd/receiver

test: ## Run all tests
	go test ./...

lint: ## Run golangci-lint
	golangci-lint run

docker-build: ## Build Docker image (TAG defaults to git short SHA)
	docker build -t $(IMAGE):$(TAG) -t $(IMAGE):latest .

docker-push: docker-build ## Build and push image to ECR
	docker push $(IMAGE):$(TAG)
	docker push $(IMAGE):latest

ecr-login: ## Authenticate Docker to ECR
	aws ecr get-login-password --region $(REGION) | \
		docker login --username AWS --password-stdin $(REGISTRY)

clean: ## Remove build artifacts
	rm -rf bin/

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  %-15s %s\n", $$1, $$2}'
