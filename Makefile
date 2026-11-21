ARCH ?= $(shell go env GOARCH)
IMAGE ?= ship:dev

.PHONY: test build image e2e-up e2e-bootstrap e2e-run e2e-down

test:
	go vet ./... && go test ./...

build:
	go build -o dist/ship ./cmd/ship

image:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o dist/linux-$(ARCH)/ship ./cmd/ship
	CGO_ENABLED=0 GOOS=linux GOARCH=$(ARCH) go build -o dist/linux-$(ARCH)/pushfreight ./e2e/tools/pushfreight
	docker build --build-arg TARGETARCH=$(ARCH) -t $(IMAGE) .

e2e-up:
	cd e2e && docker compose up -d --wait

e2e-bootstrap: image
	cd e2e && ./bootstrap.sh

e2e-run: image
	cd e2e && ./run-scenario.sh

e2e-down:
	cd e2e && docker compose down -v
