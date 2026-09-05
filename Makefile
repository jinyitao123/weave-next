.PHONY: build test test-integration vet depguard base-depguard budgetguard governance-test productguard ui-check ui-embed compose-check ci docker-build docker-image

BUILD_COMMIT := $(shell commit=$$(git rev-parse HEAD 2>/dev/null || echo unknown); if [ "$$commit" != unknown ] && [ -n "$$(git status --porcelain 2>/dev/null)" ]; then commit="$$commit-dirty"; fi; echo "$$commit")

build:
	go build ./...

test:
	go test ./internal/... ./cmd/...

test-integration:
	@test -n "$${TEST_DATABASE_URL:-}" || { echo "TEST_DATABASE_URL is required for integration tests" >&2; exit 1; }
	$(MAKE) test

vet:
	go vet ./...

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

ui-check:
	npm --prefix weave-app run lint
	npm --prefix weave-app run check:tokens
	npm --prefix weave-app run build

ui-embed: ui-check
	python3 scripts/sync-webui.py

compose-check:
	WEAVE_TEST_COMPOSE=1 python3 -m unittest discover -s tools/tests -p test_governance.py -k PlatformCompose -v

ci: build vet test depguard base-depguard budgetguard productguard governance-test

docker-build:
	./scripts/refresh-weave.sh

docker-image:
	BUILD_COMMIT=$(BUILD_COMMIT) docker compose -f docker-compose.platform.yml build weave
