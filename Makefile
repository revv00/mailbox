export GO111MODULE=on

BINARY := mbox
PKG := github.com/revv00/mailfs/pkg/version

REVISION := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
REVISIONDATE := $(shell git log -1 --pretty=format:'%cd' --date short 2>/dev/null || echo "unknown")

LDFLAGS += -X $(PKG).Revision=$(REVISION) \
           -X $(PKG).RevisionDate=$(REVISIONDATE) \
           -s -w

# Lite tags to minimize binary size (roughly 25MB)
LITE_TAGS := nogateway,nowebdav,nocos,nobos,nohdfs,noibmcos,noobs,nooss,noqingstor,nosftp,noswift,noazure,nogs,noufile,nob2,nonfs,nodragonfly,nomysql,nopg,notikv,nobadger,noetcd,nocifs,notrace,nometrics,nos3

# Docker Cross-Compilation Config (Using Goreleaser-cross for Go 1.23 support)
DOCKER_IMAGE := goreleaser/goreleaser-cross:v1.23.0
DOCKER_WORKDIR := /go/src/github.com/revv00/mailfs
HOST_GOMODCACHE := $(shell go env GOMODCACHE)

DOCKER_RUN := docker run --rm \
	-v $(shell pwd):$(DOCKER_WORKDIR) \
	-v $(HOST_GOMODCACHE):/go/pkg/mod \
	-w $(DOCKER_WORKDIR) \
	-u $(shell id -u):$(shell id -g) \
	-e GOCACHE=/tmp/go-cache \
	-e GOMODCACHE=/go/pkg/mod \
	-e GOPROXY=off \
	-e CGO_ENABLED=1 \
	--entrypoint /bin/bash $(DOCKER_IMAGE) -c

.PHONY: all mbox lite clean docker-lite-linux docker-lite-darwin-amd64 docker-lite-darwin-arm64 docker-lite-windows-amd64 docker-lite-all test-single test-multi test-del

all: mbox

mbox:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/mbox

lite:
	go build -tags $(LITE_TAGS) -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/mbox

# Docker-based cross-compilation (Lite versions)
docker-lite-linux:
	$(DOCKER_RUN) "GOOS=linux GOARCH=amd64 go build -tags $(LITE_TAGS) -ldflags=\"$(LDFLAGS)\" -o $(BINARY)-linux-amd64 ./cmd/mbox"

docker-lite-darwin-amd64:
	$(DOCKER_RUN) "GOOS=darwin GOARCH=amd64 CC=o64-clang go build -tags $(LITE_TAGS) -ldflags=\"$(LDFLAGS)\" -o $(BINARY)-darwin-amd64 ./cmd/mbox"

docker-lite-darwin-arm64:
	$(DOCKER_RUN) "GOOS=darwin GOARCH=arm64 CC=oa64-clang go build -tags $(LITE_TAGS) -ldflags=\"$(LDFLAGS)\" -o $(BINARY)-darwin-arm64 ./cmd/mbox"

docker-lite-windows-amd64:
	$(DOCKER_RUN) "GOOS=windows GOARCH=amd64 CC=x86_64-w64-mingw32-gcc CGO_CFLAGS=\"-I$(DOCKER_WORKDIR)/include/fuse\" go build -tags $(LITE_TAGS) -ldflags=\"$(LDFLAGS)\" -o $(BINARY)-amd64.exe ./cmd/mbox"

docker-lite-all: docker-lite-linux docker-lite-darwin-amd64 docker-lite-darwin-arm64 docker-lite-windows-amd64

clean:
	rm -f $(BINARY) *.exe $(BINARY)-linux-amd64 $(BINARY)-darwin-amd64 $(BINARY)-darwin-arm64

test-single:
	./run_test_single.sh

test-multi:
	./run_test_multi.sh

test-del:
	./run_test_del.sh