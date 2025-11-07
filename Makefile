PREVIOUS_TAG ?= $(shell git tag -l | tail -n 1)
TAG=v0.1.12

.PHONY: build install tag update-config test bump-readme

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

bump-readme:
	@if [ -z "$(TAG)" ]; then \
		echo "Error: TAG is required. Usage: make bump-readme TAG=v0.1.10"; \
		exit 1; \
	fi
	@git fetch --tags 2>/dev/null || true
	@VERSION=$$(echo $(TAG) | sed 's/^v//'); \
	README_TAG=$$(grep -oE 'releases/download/v[0-9]+\.[0-9]+\.[0-9]+' README.md | head -n 1 | sed 's|releases/download/||'); \
	README_VERSION=$$(echo $$README_TAG | sed 's/^v//'); \
	if [ -z "$$README_TAG" ]; then \
		echo "No version tag found in download URLs, updating placeholder versions"; \
		sed -i.bak "s|v0\.0\.0|$(TAG)|g" README.md; \
		sed -i.bak "s|roc_0\.0\.0|roc_$$VERSION|g" README.md; \
		rm -f README.md.bak; \
	else \
		echo "Current tag: $(TAG)"; \
		echo "README tag: $$README_TAG"; \
		echo "Version: $$VERSION"; \
		if [ "$$README_TAG" != "$(TAG)" ]; then \
			echo "Updating README.md: $$README_TAG -> $(TAG), roc_$$README_VERSION -> roc_$$VERSION"; \
			sed -i.bak "s|releases/download/$$README_TAG|releases/download/$(TAG)|g" README.md; \
			sed -i.bak "s|roc_$$README_VERSION|roc_$$VERSION|g" README.md; \
			rm -f README.md.bak; \
		else \
			echo "README.md already contains $(TAG)"; \
		fi; \
	fi; \
	if git diff --exit-code README.md > /dev/null 2>&1; then \
		echo "README.md already up to date"; \
	else \
		echo "README.md updated successfully"; \
		git diff README.md; \
	fi