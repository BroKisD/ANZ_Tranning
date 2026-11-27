GO := go
BINARY := anz-training
BINDIR := bin
VERSION ?=

.PHONY: help build clean fmt fmt-check vet test test-race integration test-all run migrate-up migrate-status migrate-down docker-build up down logs deps e2e

help:
	@echo "Available targets:"
	@echo "  make build          Build the application binary"
	@echo "  make run            Run the application locally"
	@echo "  make deps           Download and tidy Go modules"
	@echo "  make fmt            Format Go source files"
	@echo "  make fmt-check      Check whether Go source files are formatted"
	@echo "  make vet            Run Go static analysis"
	@echo "  make test           Run unit tests with coverage"
	@echo "  make test-race      Run unit tests with the race detector"
	@echo "  make integration    Run integration tests (requires Docker)"
	@echo "  make test-all       Run unit and integration tests"
	@echo "  make migrate-up     Apply database migrations"
	@echo "  make migrate-status Show database migration status"
	@echo "  make migrate-down   Roll migrations down to VERSION"
	@echo "  make docker-build   Build the Docker image"
	@echo "  make up             Start Docker Compose services"
	@echo "  make down           Stop Docker Compose services"
	@echo "  make logs           Follow Docker Compose logs"
	@echo "  make e2e            Run the deployed server end-to-end script"
	@echo "  make clean          Remove build artifacts"

build:
	mkdir -p $(BINDIR)
	CGO_ENABLED=0 GOOS=$${GOOS:-linux} $(GO) build -ldflags="-s -w" -o $(BINDIR)/$(BINARY) .

clean:
	rm -f $(BINDIR)/$(BINARY)
	rmdir $(BINDIR) 2>/dev/null || true

deps:
	$(GO) mod download
	$(GO) mod tidy

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './.git/*')

fmt-check:
	@files=$$(gofmt -l $$(find . -name '*.go' -not -path './.git/*')); \
	if [ -n "$$files" ]; then \
		echo "Unformatted files:"; \
		echo "$$files"; \
		exit 1; \
	fi

vet:
	$(GO) vet ./...

test:
	$(GO) test -v -cover ./...

test-race:
	$(GO) test -race ./...

integration:
	$(GO) test -v -tags=integration ./...

test-all: test integration

run:
	$(GO) run . serve

migrate-up:
	$(GO) run . migrate up

migrate-status:
	$(GO) run . migrate status

migrate-down:
	@test -n "$(VERSION)" || (echo "Usage: make migrate-down VERSION=<version>" >&2; exit 1)
	$(GO) run . migrate down-to $(VERSION)

docker-build:
	docker build -t $(BINARY) .

up:
	docker compose up --build

down:
	docker compose down

logs:
	docker compose logs -f

e2e:
	./scripts/test-server.sh
