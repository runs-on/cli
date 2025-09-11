PREVIOUS_TAG ?= $(shell git tag -l | tail -n 1)
TAG=v0.1.4

.PHONY: build install

build:
	mkdir -p dist
	go build -o dist/roc .

install: build
	sudo install -m 755 dist/roc /usr/local/bin/roc

bump:
	gsed -i "s/$(PREVIOUS_TAG)/$(TAG)/g" README.md

tag: bump
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin $(TAG)

release: tag
	gh release create $(TAG) --generate-notes
