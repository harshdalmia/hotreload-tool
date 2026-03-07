.PHONY: build test

build:
	go build -o ./bin/hotreload ./cmd/hotreload

test:
	go test ./... -count=1
