.PHONY: install build test test-integration vet depguard base-depguard budgetguard governance-test productguard workbench-install workbench-build workbench-check compose-check ci docker-build docker-image workbench-image docker-images

BUILD_COMMIT := $(shell commit=$$(git rev-parse HEAD 2>/dev/null || echo unknown); if [ "$$commit" != unknown ] && [ -n "$$(git status --porcelain 2>/dev/null)" ]; then commit="$$commit-dirty"; fi; echo "$$commit")
WEAVE_VERSION := $(shell tr -d '[:space:]' < VERSION)
PNPM ?= pnpm
# Archived acceptance probes under docs are standalone programs, not module packages.
GO_PACKAGES := ./internal/... ./cmd/... ./tools/...

install:
	./scripts/install-weave.sh

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

workbench-install:
	cd workbench && CI=true $(PNPM) install --frozen-lockfile

workbench-build:
	cd workbench && DSH_CLIENT_COMMIT_HASH=$$(git rev-parse --short=7 HEAD) DSH_CLIENT_VERSION=$(WEAVE_VERSION) $(PNPM) run build:workbench

workbench-check: workbench-build
	cd workbench && $(PNPM) exec vitest run --config vitest.e2e.config.ts apps/cli/tests/profiles/workbench.e2e.ts
	cd workbench && $(PNPM) exec vitest run packages/bundle/workbench-app/tests packages/client/ui-weave/tests packages/client/ui-layout/tests

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

workbench-image: docker-image
	docker build -f Dockerfile.workbench \
		--build-arg WEAVE_IMAGE=weave-platform \
		--build-arg DSH_CLIENT_COMMIT_HASH=$$(git rev-parse --short=7 HEAD) \
		--build-arg DSH_CLIENT_VERSION=$(WEAVE_VERSION) \
		-t weave-workbench .

docker-images: workbench-image
