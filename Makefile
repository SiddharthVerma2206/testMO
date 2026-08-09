BINARY=testmo-agent
VERSION?=0.1.0

# Always cross-compiles for the Ubuntu servers, whatever the host OS is.
# CGO off so the binary is static and doesn't care about the target's libc.
build:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags "-X main.Version=$(VERSION)" -o $(BINARY) .

vet:
	GOOS=linux GOARCH=amd64 go vet ./...

test:
	go test ./...

clean:
	rm -f $(BINARY)

.PHONY: build vet test clean
