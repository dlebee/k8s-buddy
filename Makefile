VERSION ?= 0.1.0
APP     := dist/k8s-buddy.app

.PHONY: build run headless app test vet clean

build:
	go build -o bin/k8s-buddy ./cmd/k8s-buddy

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
	sed 's/VERSION/$(VERSION)/g' packaging/macos/Info.plist > $(APP)/Contents/Info.plist
	codesign --force --sign - $(APP)
	@echo "built $(APP) — open it with: open $(APP)"

clean:
	rm -rf bin dist
