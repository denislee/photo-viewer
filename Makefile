.PHONY: all build test clean run fmt vet

# Binaries
APP_BIN=photo-viewer
SCAN_BIN=pv-scan

all: build

build:
	go build -o $(APP_BIN) main.go
	@if [ -f "./cmd/pv-scan/main.go" ]; then go build -o $(SCAN_BIN) ./cmd/pv-scan/main.go; fi

test:
	go test ./...

clean:
	go clean
	rm -f $(APP_BIN) $(SCAN_BIN)

run: build
	./$(APP_BIN)

fmt:
	go fmt ./...

vet:
	go vet ./...
