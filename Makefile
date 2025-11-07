PREVIOUS_TAG ?= $(shell git tag -l | tail -n 1)
TAG=v0.1.11

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
	ALL_TAGS=$$(git tag -l | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$$' | sort -V); \
	PREVIOUS_TAG=$$(echo "$$ALL_TAGS" | grep -v "^$(TAG)$$" | tail -n 1); \
	if [ -z "$$PREVIOUS_TAG" ]; then \
		PREVIOUS_TAG=$$(echo "$$ALL_TAGS" | tail -n 2 | head -n 1); \
	fi; \
	echo "Current tag: $(TAG)"; \
	echo "Previous tag: $$PREVIOUS_TAG"; \
	echo "Version: $$VERSION"; \
	if [ -n "$$PREVIOUS_TAG" ] && [ "$$PREVIOUS_TAG" != "$(TAG)" ]; then \
		PREVIOUS_VERSION=$$(echo $$PREVIOUS_TAG | sed 's/^v//'); \
		echo "Updating README.md: $$PREVIOUS_TAG -> $(TAG), roc_$$PREVIOUS_VERSION -> roc_$$VERSION"; \
		sed -i.bak "s|$$PREVIOUS_TAG|$(TAG)|g" README.md; \
		sed -i.bak "s|roc_$$PREVIOUS_VERSION|roc_$$VERSION|g" README.md; \
		rm -f README.md.bak; \
	else \
		echo "No previous tag found or tags are the same, updating placeholder versions"; \
		sed -i.bak "s|v0\.0\.0|$(TAG)|g" README.md; \
		sed -i.bak "s|roc_0\.0\.0|roc_$$VERSION|g" README.md; \
		rm -f README.md.bak; \
	fi; \
	if git diff --exit-code README.md > /dev/null 2>&1; then \
		echo "README.md already up to date"; \
	else \
		echo "README.md updated successfully"; \
		git diff README.md; \
	fi