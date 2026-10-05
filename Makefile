.PHONY: build test test-integration vet depguard base-depguard budgetguard governance-test productguard compose-check ci docker-build docker-image

BUILD_COMMIT := $(shell commit=$$(git rev-parse HEAD 2>/dev/null || echo unknown); if [ "$$commit" != unknown ] && [ -n "$$(git status --porcelain 2>/dev/null)" ]; then commit="$$commit-dirty"; fi; echo "$$commit")
WEAVE_VERSION := $(shell tr -d '[:space:]' < VERSION)
GO_PACKAGES := ./internal/... ./cmd/... ./tools/...

build:
	go build $(GO_PACKAGES)

test:
	go test ./internal/... ./cmd/...

test-integration:
	@test -n "$${TEST_DATABASE_URL:-}" || { echo "TEST_DATABASE_URL is required for integration tests" >&2; exit 1; }
	$(MAKE) test

vet:
	go vet $(GO_PACKAGES)

depguard:
	./scripts/depguard.sh

base-depguard:
	./scripts/base-depguard.sh

budgetguard:
	./scripts/budgetguard.sh

productguard:
	./scripts/productguard.sh

governance-test:
	python3 -m unittest discover -s tools/tests -v

compose-check:
	WEAVE_TEST_COMPOSE=1 python3 -m unittest discover -s tools/tests -p test_governance.py -k PlatformCompose -v

ci: build vet test depguard base-depguard budgetguard productguard governance-test

docker-build:
	./scripts/refresh-weave.sh


docker-image:
	docker build \
		--build-arg BUILD_COMMIT=$(BUILD_COMMIT) \
		--build-arg WEAVE_VERSION=$(WEAVE_VERSION) \
		-t weave-platform .
