.PHONY: build run clean test

build:
	@echo "Building..."
	go build .


test:
	@echo "Running tests..."
	go test -v ./...

clean:
	@echo "Clean..."
	rm data/faucet.db
	rm faucet
