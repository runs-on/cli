VERSION ?= $(shell if [ -f ../VERSION ]; then tr -d '\n' < ../VERSION; elif [ -f VERSION ]; then tr -d '\n' < VERSION; elif git describe --tags --exact-match >/dev/null 2>&1; then git describe --tags --exact-match; else echo dev; fi)
VERSION_NO_V = $(patsubst v%,%,$(VERSION))
LDFLAGS = -s -w -X roc/internal/version.Version=$(VERSION)
MONOREPO_ROOT := ..

.PHONY: build install lint test sync-metadata version

build:
	mkdir -p dist
	mise exec -- go build -ldflags="$(LDFLAGS)" -o dist/roc .

install: build
	sudo install -m 755 dist/roc /usr/local/bin/roc

lint:
	@$(MAKE) -C $(MONOREPO_ROOT) lint-cli-module

test:
	mise exec -- go test -count=1 ./...

sync-metadata:
	@VERSION="$(VERSION)" VERSION_NO_V="$(VERSION_NO_V)" perl -0pi -e 's{releases/download/v[0-9A-Za-z.\\-]+}{releases/download/$$ENV{VERSION}}g; s{roc_[0-9A-Za-z.\\-]+_(darwin|linux|windows)_(amd64|arm64)(\\.exe)?}{"roc_".$$ENV{VERSION_NO_V}."_".$$1."_".$$2.($$3 // "")}ge; s{rev: v[0-9A-Za-z.\\-]+}{rev: $$ENV{VERSION}}g' README.md
	@VERSION="$(VERSION)" perl -0pi -e 's{e\\.g\\., v[0-9A-Za-z.\\-]+}{e.g., $$ENV{VERSION}}g' action.yml

version:
	@echo $(VERSION)
