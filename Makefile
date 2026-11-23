ARCH ?= $(shell go env GOARCH)
IMAGE ?= ship:dev

.PHONY: test build image forgetest e2e-up e2e-bootstrap e2e-run e2e-down

test:
	go vet ./... && go test ./...

# Shared forge contract suite (internal/forge/forgetest) against the sandbox GitLab.
forgetest:
	set -a && . e2e/.state.env && set +a && \
	SHIP_FORGETEST_GITLAB_URL=http://localhost:8929 SHIP_FORGETEST_GITLAB_TOKEN=$$ROOT_TOKEN \
	go test -count=1 -run TestForgeContract -v ./internal/gitlab

build:
	go build -o dist/ship ./cmd/ship

image:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o dist/linux-$(ARCH)/ship ./cmd/ship
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o dist/linux-$(ARCH)/pushfreight ./e2e/tools/pushfreight
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o dist/linux-$(ARCH)/fakes ./e2e/tools/fakes
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMAGE) .

e2e-up:
	cd e2e && docker compose up -d --wait

e2e-bootstrap: image
	cd e2e && ./bootstrap.sh

e2e-run: image
	cd e2e && ./run-scenario.sh

e2e-down:
	cd e2e && docker compose down -v
