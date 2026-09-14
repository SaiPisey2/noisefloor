.PHONY: build test demo-up demo-down demo-logs integration

build:
	CGO_ENABLED=0 go build -o noisefloor ./cmd/noisefloor

test:
	go test ./...

demo-up:
	docker-compose -f demo/docker-compose.yml up -d --build
	@echo "prometheus   http://localhost:9090"
	@echo "alertmanager http://localhost:9093"

demo-down:
	docker-compose -f demo/docker-compose.yml down -v

demo-logs:
	docker-compose -f demo/docker-compose.yml logs -f

integration:
	go test ./demo/... -tags=integration -v
