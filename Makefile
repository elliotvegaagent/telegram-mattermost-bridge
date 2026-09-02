.PHONY: test check build

test:
	go test ./...

check:
	go test -race ./...
	go vet ./...

build:
	CGO_ENABLED=0 go build -trimpath -o dist/bridge ./cmd/bridge
