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

# ./... rather than ./demo/...: the end-to-end scan test lives in internal/e2e,
# and a target scoped to ./demo/... silently ran none of it.
#
# -count=1 is mandatory, not decorative: without it Go's test cache can return
# a PASS for an integration target that never touched the running stack.
integration:
	go test ./... -tags=integration -v -count=1

.PHONY: demo-seed

# Generate synthetic ALERTS history and convert it to TSDB blocks that the
# demo Prometheus loads on start.
demo-seed:
	rm -rf demo/seed/blocks demo/seed/alerts.openmetrics
	go run ./demo/seed -out demo/seed/alerts.openmetrics -days 30
	docker run --rm -v $(PWD)/demo/seed:/seed --entrypoint promtool prom/prometheus:v3.6.0 \
		tsdb create-blocks-from openmetrics /seed/alerts.openmetrics /seed/blocks
	docker-compose -f demo/docker-compose.yml stop prometheus
	# Reset history before copying. Without this the volume ACCUMULATES: every
	# re-seed leaves the previous run's blocks in place, so the same rule ends
	# up with one series per seeding, episode counts multiply, and a label
	# added between runs shows up as a second fingerprint for the same rule.
	# demo-seed is a reset, not an append.
	docker run --rm -v demo_promdata:/dst alpine \
		sh -c 'rm -rf /dst/01* /dst/wal /dst/chunks_head'
	docker run --rm -v $(PWD)/demo/seed/blocks:/src -v demo_promdata:/dst alpine \
		sh -c 'cp -r /src/* /dst/'
	docker-compose -f demo/docker-compose.yml start prometheus
	@echo "seeded 30d of ALERTS history (previous history discarded)"
