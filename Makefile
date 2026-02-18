BINARY   := sticky-recorder
MODULE   := github.com/whisper-darkly/sticky-recorder
VERSION  := $(shell cat VERSION)
LDFLAGS  := -s -w -X main.version=$(VERSION)
DISTDIR  := dist

.PHONY: all build install clean

all: build

build:
	@mkdir -p $(DISTDIR)
	go build -ldflags '$(LDFLAGS)' -o $(DISTDIR)/$(BINARY) .

install: build
	install -d $(DESTDIR)$(PREFIX)/bin
	install -m 755 $(DISTDIR)/$(BINARY) $(DESTDIR)$(PREFIX)/bin/$(BINARY)

clean:
	rm -rf $(DISTDIR)

PREFIX ?= /usr/local
DESTDIR ?=
