REGISTRY_TARGET ?= quay

.PHONY: build build-driver test lint fmt vet docker-driver clean

build: build-driver ## build the driver binary

build-driver: ## build the iSCSI CSI driver binary
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
		go build -o cmd/csi-driver/iscsi-csi-ngxstorage-driver ./cmd/csi-driver

test: ## run unit tests
	go test ./...

lint: fmt vet ## format, vet (lint gate)

fmt: ## gofmt check
	@test -z "$$(gofmt -l pkg cmd)" || { echo "gofmt needed:"; gofmt -l pkg cmd; exit 1; }

vet: ## go vet
	go vet ./...

docker-driver: ## build the commit-tagged driver image without pushing
	python3 build/image.py local

.PHONY: docker-push
docker-push: ## build and push the commit tag to Quay (clean sources required)
	python3 build/image.py "$(REGISTRY_TARGET)"

clean: ## remove build artifacts
	rm -f cmd/csi-driver/iscsi-csi-ngxstorage-driver

.PHONY: docker-all
docker-all: docker-driver ## compatibility alias for the driver-only image build

.PHONY: digests ci
digests: ## print the driver image digest
	@cat build/artifacts/image.json

ci: lint build ## local driver CI gate
