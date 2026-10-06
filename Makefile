.PHONY: all lint vet test test-integration golden-update ci fmt

all: test

fmt:
	go fmt ./...
	(cd otelgrizzle && go fmt ./...)

vet:
	go vet ./...
	(cd otelgrizzle && go vet ./...)

lint:
	golangci-lint run ./...
	(cd otelgrizzle && golangci-lint run ./...)

test:
	go test -race -count=1 ./...
	(cd otelgrizzle && go test -race -count=1 ./...)

test-integration:
	@echo "Running integration tests..."
	@out=$$(go test -tags integration -race -count=1 -v ./...); \
	echo "$$out"; \
	pass_count=$$(echo "$$out" | grep -c "^--- PASS" || true); \
	echo "Executed passing integration tests: $$pass_count"; \
	if [ "$$pass_count" -eq 0 ]; then \
		echo "Error: No integration tests were executed! Ensure PostgreSQL is running under integration tag."; \
		exit 1; \
	fi

golden-update:
	go test -run TestPlan_GoldenFile -update .

ci: lint vet test test-integration
