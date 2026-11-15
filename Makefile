VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)
BINDIR  ?= $(HOME)/.local/bin

.PHONY: all build run test lint install snapshot icons clean

all: build

build:
	CGO_ENABLED=1 go build -trimpath -ldflags "$(LDFLAGS)" -o navigo ./cmd/navigo

run: build
	./navigo

test:
	go test -race ./...

lint:
	go vet ./...
	@out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

install: build
	install -Dm755 navigo $(BINDIR)/navigo
	$(BINDIR)/navigo install

snapshot:
	goreleaser release --snapshot --clean

icons:
	go run ./tools/genicon

clean:
	rm -rf navigo dist
