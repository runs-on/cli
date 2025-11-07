PREVIOUS_TAG ?= $(shell git tag -l | tail -n 1)
TAG=v0.1.9

.PHONY: build install bump tag release update-config test

build:
	mkdir -p dist
	go build -o dist/roc .

install: build
	sudo install -m 755 dist/roc /usr/local/bin/roc

tag:
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin $(TAG)

test:
	go test ./...