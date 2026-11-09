BINARY = prometheus-msteams
GOARCH = amd64
BINDIR = bin
VERSION:=$(shell git describe --tags --always --dirty)
COMMIT=$(shell git rev-parse --short HEAD)
BRANCH=$(shell git rev-parse --abbrev-ref HEAD)
BUILD_DATE=$(shell date +%FT%T%z)

VERSION_PKG=github.com/aursu/prometheus-msteams/pkg/version

# Setup the -ldflags option for go build here, interpolate the variable values
LDFLAGS = -ldflags "-X $(VERSION_PKG).VERSION=$(VERSION) -X $(VERSION_PKG).COMMIT=$(COMMIT) -X $(VERSION_PKG).BRANCH=$(BRANCH) -X $(VERSION_PKG).BUILDDATE=$(BUILD_DATE)"

RUN_ARGS ?=

# Build the project
all: clean dep create_bin_dir linux darwin windows
	cd $(BINDIR) && shasum -a 256 ** > shasum256.txt

create_bin_dir:
	rm -fr $(BINDIR)
	mkdir -p $(BINDIR)

linux:
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build $(LDFLAGS) -o $(BINDIR)/$(BINARY)-linux-$(GOARCH) ./cmd/server

darwin:
	CGO_ENABLED=0 GOOS=darwin GOARCH=$(GOARCH) go build $(LDFLAGS) -o $(BINDIR)/$(BINARY)-darwin-$(GOARCH) ./cmd/server

windows:
	CGO_ENABLED=0 GOOS=windows GOARCH=$(GOARCH) go build $(LDFLAGS) -o $(BINDIR)/$(BINARY)-windows-$(GOARCH).exe ./cmd/server

# The image compiles the program itself (see Dockerfile); no local Go needed.
docker:
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		--build-arg BRANCH=$(BRANCH) \
		--build-arg BUILD_DATE=$(BUILD_DATE) \
		-t $(BINARY):$(VERSION) .

run:
	go run ./cmd/server -http-addr=localhost:2000 $(RUN_ARGS)

fmt:
	gofmt -w .

lint:
	golangci-lint run ./...

test:
	go test ./... -race

coverage:
	go test ./... -race -coverprofile=coverage.txt -covermode=atomic

dep:
	go mod tidy
	go mod download

clean:
	-rm -fr $(BINDIR)
	-rm -f $(BINARY)-*
