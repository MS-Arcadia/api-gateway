# api-gateway tasks. Run `make help` for the list.

SERVICE := api-gateway
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo local)
IMAGE   ?= arcadia/$(SERVICE)

.DEFAULT_GOAL := help
.PHONY: help build run test cover vet lint fmt tidy docker routes clean ci

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'

build: ## Compile the binary into ./bin
	go build -trimpath -ldflags="-s -w" -o bin/$(SERVICE) ./cmd/$(SERVICE)

run: ## Run locally against the compose stack (cd ../infra && make up)
	go run ./cmd/$(SERVICE)

# The gateway holds no data, so `make test` is the whole suite — there is no
# integration tier to separate out.
test: ## Tests with the race detector. Needs no infrastructure.
	go test -race -count=1 ./...

cover: ## Tests with a coverage report
	go test -race -count=1 -coverprofile=coverage.out ./...
	@go tool cover -func=coverage.out | tail -20

vet: ## go vet
	go vet ./...

lint: vet ## Vet plus staticcheck when it is installed
	@command -v staticcheck >/dev/null 2>&1 \
		&& staticcheck ./... \
		|| echo "staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@latest)"

fmt: ## Format
	gofmt -s -w .

tidy: ## Tidy and verify the module graph
	go mod tidy && go mod verify

docker: ## Build the container image
	@# The host's GOPROXY is handed to the build as a secret rather than a build
	@# argument, because it may carry credentials and a build argument would be
	@# readable in `docker history`. Needed at all because the public proxy is often
	@# unreachable from inside the build VM.
	GOPROXY="$$(go env GOPROXY)" docker build \
		--secret id=goproxy,env=GOPROXY \
		--build-arg VERSION=$(VERSION) \
		-t $(IMAGE):local -t $(IMAGE):$(VERSION) .

routes: ## Print the routing table of a running gateway
	@curl -fsS localhost:$${HTTP_PORT:-8090}/ | python3 -m json.tool

ci: tidy lint test ## Everything the pipeline runs

clean: ## Remove build artefacts
	rm -rf bin coverage.out
