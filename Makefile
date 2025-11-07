PREVIOUS_TAG ?= $(shell git tag -l | tail -n 1)
TAG=v0.1.10

.PHONY: build install bump tag release update-config test

build:
	mkdir -p dist
	go build -o dist/roc .

install: build
	sudo install -m 755 dist/roc /usr/local/bin/roc

tag:
	git commit -m "Bump version to $(TAG)" Makefile
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin main && git push origin $(TAG)

test:
	go test ./...