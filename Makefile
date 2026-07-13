.PHONY: all build test clean run fmt vet

# Binaries
APP_BIN=photo-viewer
SCAN_BIN=pv-scan
ORGANIZE_BIN=pv-organize
EXPORT_FAV_BIN=pv-export-favorites

all: build

build:
	go build -o $(APP_BIN) .
	go build -o $(SCAN_BIN) ./cmd/pv-scan
	go build -o $(ORGANIZE_BIN) ./cmd/pv-organize
	go build -o $(EXPORT_FAV_BIN) ./cmd/pv-export-favorites
	@if [ -f "./scripts/pv-face-detect.py" ]; then install -m 0755 ./scripts/pv-face-detect.py ./pv-face-detect; fi

test:
	go test ./...

clean:
	go clean
	rm -f $(APP_BIN) $(SCAN_BIN) $(ORGANIZE_BIN) $(EXPORT_FAV_BIN) ./pv-face-detect

run: build
	./$(APP_BIN)

fmt:
	go fmt ./...

vet:
	go vet ./...
