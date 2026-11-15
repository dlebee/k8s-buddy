# The version comes from git tags (v1.2.3 → 1.2.3); untagged builds get a describe string.
VERSION ?= $(shell git describe --tags --match 'v*' --always --dirty 2>/dev/null | sed 's/^v//' || echo dev)
# macOS bundle versions must be plain numbers, so drop any pre-release/describe suffix there.
PLIST_VERSION = $(shell echo '$(VERSION)' | sed -E 's/^([0-9]+\.[0-9]+(\.[0-9]+)?)([^0-9.].*)?$$/\1/; /^[0-9]+\.[0-9]+(\.[0-9]+)?$$/!s/.*/0.0.0/')
APP     := dist/k8s-buddy.app

.PHONY: build run headless app app-universal test vet clean

build:
	go build -ldflags "-X main.version=$(VERSION)" -o bin/k8s-buddy ./cmd/k8s-buddy

run: build
	./bin/k8s-buddy

headless: build
	./bin/k8s-buddy --headless

test:
	go test ./...

vet:
	go vet ./...

app: build
	rm -rf $(APP)
	mkdir -p $(APP)/Contents/MacOS
	cp bin/k8s-buddy $(APP)/Contents/MacOS/k8s-buddy
	sed 's/VERSION/$(PLIST_VERSION)/g' packaging/macos/Info.plist > $(APP)/Contents/Info.plist
	codesign --force --sign - $(APP)
	@echo "built $(APP) — open it with: open $(APP)"

# Universal (Apple Silicon + Intel) bundle, as published in releases.
app-universal:
	packaging/macos/build-app.sh $(VERSION) dist

clean:
	rm -rf bin dist
