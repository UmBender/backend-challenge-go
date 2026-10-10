.PHONY: fmt fmt-check vet test
fmt:
	gofmt -w .

fmt-check:
	@test -z "$$(gofmt -l .)" || (gofmt -l . && echo "run: make fmt" && exit 1)

vet:
	go vet ./...
	go vet -tags=integration ./...

test:
	go test ./...

