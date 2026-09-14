.PHONY: build test demo-up demo-down demo-logs integration

build:
	CGO_ENABLED=0 go build -o noisefloor ./cmd/noisefloor

test:
	go test ./...

# Build and start are separate on purpose. `up -d --build` has been observed
# hanging with containers stuck in Created; building first makes the failure
# visible at the build step instead of as an indefinite hang.
demo-up:
	docker-compose -f demo/docker-compose.yml build
	docker-compose -f demo/docker-compose.yml up -d
	docker-compose -f demo/docker-compose.yml ps
	@echo "prometheus   http://localhost:9090"
	@echo "alertmanager http://localhost:9093"

demo-down:
	docker-compose -f demo/docker-compose.yml down -v

demo-logs:
	docker-compose -f demo/docker-compose.yml logs -f

# -count=1 is mandatory, not decorative: without it Go's test cache can return
# a PASS for an integration target that never touched the running stack.
integration:
	go test ./demo/... -tags=integration -v -count=1
