.PHONY: build test vet depguard base-depguard budgetguard ci docker-build docker-image

BUILD_COMMIT := $(shell commit=$$(git rev-parse HEAD 2>/dev/null || echo unknown); if [ "$$commit" != unknown ] && [ -n "$$(git status --porcelain 2>/dev/null)" ]; then commit="$$commit-dirty"; fi; echo "$$commit")

build:
	go build ./...

test:
	go test ./internal/... ./cmd/...

vet:
	go vet ./...

depguard:
	./scripts/depguard.sh

base-depguard:
	./scripts/base-depguard.sh

budgetguard:
	./scripts/budgetguard.sh

ci: build vet test depguard base-depguard budgetguard

docker-build:
	./scripts/refresh-weave.sh

docker-image:
	BUILD_COMMIT=$(BUILD_COMMIT) docker compose -f docker-compose.platform.yml build weave
