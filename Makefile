.PHONY: ui test test-db build
ui:
	cd ui && npm ci && npm run build
test:
	go vet ./...
	go test ./...
test-db:
	scripts/test-db.sh
build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...
