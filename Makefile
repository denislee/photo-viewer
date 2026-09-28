.PHONY: all build test race test-video lint vuln check clean run fmt fmt-check vet

# Binaries
APP_BIN=photo-viewer
SCAN_BIN=pv-scan
ORGANIZE_BIN=pv-organize
EXPORT_FAV_BIN=pv-export-favorites

# Pinned dev tools, run via `go run` so nothing needs installing first.
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
GOVULNCHECK   ?= go run golang.org/x/vuln/cmd/govulncheck@v1.8.0

all: build

build:
	go build -o $(APP_BIN) .
	go build -o $(SCAN_BIN) ./cmd/pv-scan
	go build -o $(ORGANIZE_BIN) ./cmd/pv-organize
	go build -o $(EXPORT_FAV_BIN) ./cmd/pv-export-favorites
	@if [ -f "./scripts/pv-face-detect.py" ]; then install -m 0755 ./scripts/pv-face-detect.py ./pv-face-detect; fi

test:
	go test -count=1 ./...

race:
	go test -race -count=1 ./...

# The libmpv Close/Render and Close/FILE_LOADED race tests only run when
# PV_CROP_VIDEO points at a real video, so plain `test`/`race` skip them.
# This generates a short clip with ffmpeg and runs just those tests.
test-video:
	@clip=$$(mktemp --suffix=.mp4) && trap 'rm -f "$$clip"' EXIT && \
	ffmpeg -loglevel error -y -f lavfi -i testsrc=duration=3:size=640x360:rate=30 -pix_fmt yuv420p "$$clip" && \
	PV_CROP_VIDEO="$$clip" go test -race -count=1 -run 'TestClose' -v ./internal/video/

lint:
	$(GOLANGCI_LINT) run ./...

vuln:
	$(GOVULNCHECK) ./...

# Everything CI runs.
check: fmt-check vet lint race test-video vuln

clean:
	go clean
	rm -f $(APP_BIN) $(SCAN_BIN) $(ORGANIZE_BIN) $(EXPORT_FAV_BIN) ./pv-face-detect

run: build
	./$(APP_BIN)

fmt:
	go fmt ./...

# Fails listing any unformatted file (third_party/ is a vendored fork).
fmt-check:
	@out=$$(gofmt -l $$(git ls-files '*.go' | grep -v '^third_party/')); \
	if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet:
	go vet ./...
