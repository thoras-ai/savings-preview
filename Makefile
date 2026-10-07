VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS := -s -w -X main.version=$(VERSION)
PLATFORMS := darwin/amd64 darwin/arm64 linux/amd64 linux/arm64

.PHONY: build demo test test-all vulncheck clean

build:
	@mkdir -p bin
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; \
		out=bin/savings-preview-$$os-$$arch; \
		echo "$$out"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "$(LDFLAGS)" -o $$out ./cmd/savings-preview || exit 1; \
	done

# test runs what CI runs on a pull request. test-all adds go vet and the race
# detector, as CI does on main, nightly and before a release.
test: demo
	go test -count=1 ./...
	python3 -m unittest discover -s tests
	node --test 'tests/**/*.test.js'
	python3 scripts/validate_contract.py fixtures/savings-preview.otlp.json fixtures/intermittent.otlp.json

test-all: demo
	go vet ./...
	go test -race -count=1 ./...
	python3 -m unittest discover -s tests
	node --test 'tests/**/*.test.js'
	python3 scripts/validate_contract.py fixtures/savings-preview.otlp.json fixtures/intermittent.otlp.json

# vulncheck downloads the Go vulnerability database, so it is not part of test.
vulncheck:
	go tool -modfile=tools/go.mod govulncheck ./...

# demo writes the demo cluster and fleet samples that the site build and its tests read.
demo:
	go run ./scripts/gen_demo
	go run ./scripts/gen_demo -fleet

clean:
	rm -rf bin
