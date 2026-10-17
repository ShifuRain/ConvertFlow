.PHONY: ui test test-db build
ui:
	cd ui && npm ci && npm run build
test: ui
	go vet ./...
	CONVERTFLOW_REQUIRE_UI=1 go test ./...
test-db:
	scripts/test-db.sh
build:
	CGO_ENABLED=0 go build -trimpath -o bin/ ./cmd/...
