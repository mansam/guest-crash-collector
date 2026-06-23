BINARY = guest-crash-collector

.PHONY: build
build:
	go build -o bin/$(BINARY) .

.PHONY: test
test:
	go test ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: clean
clean:
	rm -rf bin/
