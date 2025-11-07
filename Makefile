PREVIOUS_TAG ?= $(shell git tag -l | tail -n 1)
TAG=v0.1.7
FILES_TO_COMMIT=Makefile README.md

.PHONY: build install bump tag release update-config

build:
	mkdir -p dist
	go build -o dist/roc .

install: build
	sudo install -m 755 dist/roc /usr/local/bin/roc

bump:
	@PREVIOUS_VERSION=$$(echo $(PREVIOUS_TAG) | sed 's/^v//'); \
	NEW_VERSION=$$(echo $(TAG) | sed 's/^v//'); \
	gsed -i "s/$(PREVIOUS_TAG)/$(TAG)/g" README.md; \
	gsed -i "s/roc_$$PREVIOUS_VERSION/roc_$$NEW_VERSION/g" README.md
	if git diff --exit-code $(FILES_TO_COMMIT); then \
		echo "No changes to commit"; \
	else \
		git commit -m "Bump version to $(TAG)" $(FILES_TO_COMMIT) && \
		git push origin main && \
		git diff --exit-code; \
	fi

tag: bump
	git tag -a $(TAG) -m "Release $(TAG)"
	git push origin $(TAG)

release: tag
	gh release create $(TAG) --generate-notes --draft