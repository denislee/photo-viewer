.PHONY: all build install uninstall test race test-video lint vuln check clean run fmt fmt-check vet

# Binaries
APP_BIN=photo-viewer
SCAN_BIN=pv-scan
ORGANIZE_BIN=pv-organize
EXPORT_FAV_BIN=pv-export-favorites
FACE_BIN=pv-face-detect

# Install location. Defaults to ~/.local/bin (no sudo); override with
# `make install PREFIX=/usr/local` (needs sudo) or `make install BINDIR=...`.
# DESTDIR is honoured for staged/packaged installs.
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin
APPDIR ?= $(PREFIX)/share/applications
ICONDIR ?= $(PREFIX)/share/icons/hicolor/scalable/apps
DESKTOP_FILE=photo-viewer.desktop
ICON_FILE=assets/photo-viewer.svg

# Pinned dev tools, run via `go run` so nothing needs installing first.
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
GOVULNCHECK   ?= go run golang.org/x/vuln/cmd/govulncheck@v1.8.0

all: build

build:
	go build -o $(APP_BIN) .
	go build -o $(SCAN_BIN) ./cmd/pv-scan
	go build -o $(ORGANIZE_BIN) ./cmd/pv-organize
	go build -o $(EXPORT_FAV_BIN) ./cmd/pv-export-favorites
	@if [ -f "./scripts/pv-face-detect.py" ]; then install -m 0755 ./scripts/pv-face-detect.py ./$(FACE_BIN); fi

# Rebuilds from the current tree, then copies every binary into $(BINDIR).
# pv-face-detect is looked up on PATH (or next to photo-viewer), so it goes
# alongside the others.
install: build
	install -d "$(DESTDIR)$(BINDIR)"
	install -m 0755 $(APP_BIN) $(SCAN_BIN) $(ORGANIZE_BIN) $(EXPORT_FAV_BIN) "$(DESTDIR)$(BINDIR)/"
	@if [ -f "./$(FACE_BIN)" ]; then install -m 0755 ./$(FACE_BIN) "$(DESTDIR)$(BINDIR)/"; fi
	install -Dm 0644 $(ICON_FILE) "$(DESTDIR)$(ICONDIR)/$(APP_BIN).svg"
	install -d "$(DESTDIR)$(APPDIR)"
	@printf '%s\n' \
		'[Desktop Entry]' \
		'Type=Application' \
		'Name=Photo Viewer' \
		'GenericName=Photo & Video Viewer' \
		'Comment=Browse, view and organize photos and videos' \
		'Exec=$(BINDIR)/$(APP_BIN)' \
		'Icon=$(APP_BIN)' \
		'Terminal=false' \
		'Categories=Graphics;Photography;Viewer;' \
		'Keywords=photo;video;image;gallery;viewer;' \
		> "$(DESTDIR)$(APPDIR)/$(DESKTOP_FILE)"
	@command -v update-desktop-database >/dev/null 2>&1 && [ -z "$(DESTDIR)" ] && update-desktop-database -q "$(APPDIR)" || true
	@echo "Installed to $(DESTDIR)$(BINDIR) (+ $(DESTDIR)$(APPDIR)/$(DESKTOP_FILE))"
	@case ":$$PATH:" in *":$(BINDIR):"*) ;; *) echo "note: $(BINDIR) is not on your PATH";; esac

uninstall:
	rm -f $(addprefix "$(DESTDIR)$(BINDIR)/,$(addsuffix ",$(APP_BIN) $(SCAN_BIN) $(ORGANIZE_BIN) $(EXPORT_FAV_BIN) $(FACE_BIN))) \
		"$(DESTDIR)$(APPDIR)/$(DESKTOP_FILE)" "$(DESTDIR)$(ICONDIR)/$(APP_BIN).svg"

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

# Everything to run before pushing.
check: fmt-check vet lint race test-video vuln

clean:
	go clean
	rm -f $(APP_BIN) $(SCAN_BIN) $(ORGANIZE_BIN) $(EXPORT_FAV_BIN) ./$(FACE_BIN)

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
