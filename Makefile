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

.PHONY: all mbox lite clean linux windows darwin

all: mbox

mbox:
	go build -ldflags="$(LDFLAGS)" -o $(BINARY) ./cmd/mbox

lite:
	go build -tags $(LITE_TAGS) -ldflags="$(LDFLAGS)" -o $(BINARY).lite ./cmd/mbox

linux:
	GOOS=linux GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(BINARY)-linux-amd64 ./cmd/mbox

windows:
	GOOS=windows GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(BINARY).exe ./cmd/mbox

darwin:
	GOOS=darwin GOARCH=amd64 go build -ldflags="$(LDFLAGS)" -o $(BINARY)-darwin-amd64 ./cmd/mbox
	GOOS=darwin GOARCH=arm64 go build -ldflags="$(LDFLAGS)" -o $(BINARY)-darwin-arm64 ./cmd/mbox

clean:
	rm -f $(BINARY) $(BINARY).lite $(BINARY).exe $(BINARY)-linux-amd64 $(BINARY)-darwin-amd64 $(BINARY)-darwin-arm64
