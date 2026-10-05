.DEFAULT_GOAL := build

.PHONY: build check fmt fmt-check test vet

# Build the executable; bin/ is ignored by Git.
build:
	go build -o bin/curator ./cmd/curator

# Run the repository's formatting, static-analysis, and race checks.
check: fmt-check vet test

fmt:
	gofmt -w cmd internal

fmt-check:
	@files="$$(gofmt -l cmd internal)" || exit $$?; \
	if [ -n "$$files" ]; then printf '%s\n' "$$files"; exit 1; fi

vet:
	go vet ./...

test:
	go test -race ./...
