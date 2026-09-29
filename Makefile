# Posthorn developer tasks. Go sources live in core/; recipes cd there.
#
# Testing tiers (issue #76):
#   make test        Hermetic — no network, no credentials. Runs on every PR.
#   make test-live   Live tier — real provider APIs against non-delivering
#                    targets. Postmark uses its PUBLIC test token (zero
#                    secret); other providers SKIP unless their env vars are
#                    set. Source real keys from your secret store, e.g.:
#                      RESEND_API_KEY=$$(pass show posthorn/resend-test) \
#                      MAILGUN_API_KEY=$$(pass show posthorn/mailgun-test) \
#                      MAILGUN_DOMAIN=sandboxXXX.mailgun.org \
#                      make test-live
#
# No third-party key is ever required to run `make test`.

.PHONY: test test-live validate-lifecycle lint fmt vet cover build site help
.DEFAULT_GOAL := help

test: ## Hermetic test suite with the race detector (default PR gate)
	cd core && go test -race -count=1 -timeout=3m ./...

test-live: ## Live tier (-tags integration): real APIs, non-delivering targets
	cd core && go test -tags integration -race -count=1 -timeout=5m ./providertest/...

validate-lifecycle: ## Story 18.3: live lifecycle validation (real Postmark + quick tunnel; needs POSTMARK_SERVER_TOKEN, POSTHORN_TEST_FROM/TO, cloudflared)
	cd core && go test -tags lifecyclelive -count=1 -v -timeout=15m -run TestLifecycleLive ./providertest/

lint: ## golangci-lint
	cd core && golangci-lint run

fmt: ## Fail if any file needs gofmt
	@cd core && out="$$(gofmt -l .)"; if [ -n "$$out" ]; then echo "gofmt needed:"; echo "$$out"; exit 1; fi

vet: ## go vet
	cd core && go vet ./...

cover: ## Coverage summary
	cd core && go test -count=1 -cover ./...

build: ## Build the posthorn binary
	cd core && go build -o ../bin/posthorn ./cmd/posthorn

site: ## Build the docs site
	cd site && npm ci && npm run build

help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2}'
