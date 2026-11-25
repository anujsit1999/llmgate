.PHONY: help run test cover lint vuln build docker keygen fmt

help:  ## Show targets
	@grep -E '^[a-z]+:.*##' $(MAKEFILE_LIST) | awk -F':.*## ' '{printf "  %-8s %s\n", $$1, $$2}'

run:   ## Run the gateway with ./config.yaml
	go run ./cmd/llmgate -config config.yaml

test:  ## Unit tests with the race detector
	go test -race -count=1 ./...

cover: ## Coverage report
	go test -coverprofile=cover.out ./... && go tool cover -func=cover.out | tail -1

lint:  ## Static analysis (needs golangci-lint)
	golangci-lint run

vuln:  ## Known-vulnerability scan of dependencies
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

fmt:   ## Format code
	gofmt -w .

build: ## Build binaries into ./bin
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...

docker: ## Build the container image
	docker build -t llmgate:dev .

keygen: ## Generate a client API key
	go run ./cmd/llmgate-keygen
