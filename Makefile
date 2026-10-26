.PHONY: build test vet fmt check clean
build:
	go build -o bin/infestation ./cmd/infestation

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w cmd internal infra/azure/main.go infra/qemu/main.go

check: test vet
	@test -z "$$(gofmt -l cmd internal infra/azure/main.go infra/qemu/main.go)" || (echo 'Run make fmt'; exit 1)

clean:
	go clean ./...
