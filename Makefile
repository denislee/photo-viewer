.PHONY: all build install face-deps check-face uninstall test race test-video test-py lint vuln check clean run fmt fmt-check vet

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

# One go build for every binary so they compile and link in parallel. With
# -o pointing at a directory, each main package is written there under its
# directory name: photo-viewer (the module root), pv-scan, pv-organize and
# pv-export-favorites — the *_BIN names above.
build: $(FACE_BIN)
	go build -o . . ./cmd/...

# The face helper is the Python script, installed executable under its
# command name; a file rule so check-face never tests a stale copy.
$(FACE_BIN): scripts/pv-face-detect.py
	install -m 0755 $< $@

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
	@[ -n "$(DESTDIR)" ] || $(MAKE) --no-print-directory check-face FACE_HELPER="$(BINDIR)/$(FACE_BIN)"

# Runs the face helper's self-test (the same --check the GUI's Recheck runs)
# and explains the fix when it fails. Never fails the build: faces are
# optional, the GUI just keeps the People feature off.
FACE_HELPER ?= ./$(FACE_BIN)
check-face: $(FACE_BIN)
	@if [ ! -x "$(FACE_HELPER)" ]; then echo "face helper: $(FACE_HELPER) not built"; \
	elif out=$$("$(FACE_HELPER)" --check 2>&1); then echo "face helper: $$out"; \
	else echo "warning: face detection stays off until the helper works:"; \
		echo "$$out" | sed 's/^/  /'; fi

# (Re)builds the Python side of face detection in a private venv
# ($(FACE_VENV), where pv-face-detect looks first) using only python3's stdlib
# venv, so pipx is not needed. pip builds dlib from source against the
# system's shared libraries (needs cmake and a C++ compiler, and takes
# several minutes), so a system upgrade that drops one of them breaks the
# venv with "cannot open shared object file"; --clear and --no-cache-dir
# make the rebuild relink instead of reusing the stale wheel.
# face_recognition_models still imports pkg_resources, which setuptools 81
# removed, hence the pin. dlib turns CUDA on whenever it finds a toolkit, and
# that build fails to import when the driver lags the toolkit ("CUDA driver
# version is insufficient"); the helper runs the CPU HOG detector, so CUDA is
# off unless DLIB_USE_CUDA=1. The last step tests the venv's own interpreter:
# a working system Python must not mask a broken venv.
FACE_VENV ?= $(or $(XDG_DATA_HOME),$(HOME)/.local/share)/photo-viewer/face-venv
DLIB_USE_CUDA ?= 0
face-deps: $(FACE_BIN)
	@for t in cmake c++; do command -v $$t >/dev/null || { echo "face-deps: $$t not found; dlib builds from source (Arch: sudo pacman -S --needed cmake base-devel)"; exit 1; }; done
	python3 -m venv --clear "$(FACE_VENV)"
	DLIB_USE_CUDA=$(DLIB_USE_CUDA) CMAKE_BUILD_PARALLEL_LEVEL=$$(nproc) \
		"$(FACE_VENV)/bin/python" -m pip install --no-cache-dir 'setuptools<81' face_recognition
	"$(FACE_VENV)/bin/python" ./$(FACE_BIN) --check && echo
	@$(MAKE) --no-print-directory check-face

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
	PV_CROP_VIDEO="$$clip" go test -race -count=1 -run 'TestClose' -v ./internal/video/ && \
	PV_CROP_VIDEO="$$clip" go test -race -count=1 -run 'TestPlayVideoPaintSizeMatchesFrame' -v ./internal/ui/

# The Python helper's tests drive pipx venv selection, which they can only
# exercise when face_recognition is NOT importable in the runner, so they run
# in a throwaway venv without system site-packages.
test-py:
	@d=$$(mktemp -d) && trap 'rm -rf "$$d"' EXIT && \
	python3 -m venv --without-pip "$$d" && "$$d/bin/python" scripts/test_pv_face_detect.py

lint:
	$(GOLANGCI_LINT) run ./...

vuln:
	$(GOVULNCHECK) ./...

# Everything to run before pushing.
check: fmt-check vet lint race test-video test-py vuln

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
