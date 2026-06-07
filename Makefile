.PHONY: help run build test fmt vet check clean

BINARY ?= ai-proxy
CMD ?= ./cmd/ai-proxy
GO ?= go
GOFLAGS ?= -buildvcs=false

help:
	@printf '%s\n' 'targets:'
	@printf '  %-10s %s\n' 'run' 'run ai-proxy locally'
	@printf '  %-10s %s\n' 'build' 'build the ai-proxy binary'
	@printf '  %-10s %s\n' 'test' 'run all Go tests'
	@printf '  %-10s %s\n' 'fmt' 'format Go source files'
	@printf '  %-10s %s\n' 'vet' 'run go vet'
	@printf '  %-10s %s\n' 'check' 'run fmt, vet, and test'
	@printf '  %-10s %s\n' 'clean' 'remove build artifacts'

run:
	$(GO) run $(GOFLAGS) $(CMD)

build:
	$(GO) build $(GOFLAGS) -o $(BINARY) $(CMD)

test:
	$(GO) test ./...

fmt:
	$(GO) fmt ./...

vet:
	$(GO) vet ./...

check: fmt vet test

clean:
	$(GO) clean
	rm -f $(BINARY)
