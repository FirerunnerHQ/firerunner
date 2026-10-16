BINARY := firerunner
PKG := ./...
# Run ALL packages, including listener/CLI wiring tests. The aggregate coverage
# gate covers the functional core and host identity resolver; the imperative
# listener/CLI shell is tested but excluded from the aggregate denominator.
COVERPKG := ./internal/config/...,./internal/core/...,./internal/provisioner/...,./internal/scheduler/...,./internal/diag/...,./internal/cacheserver/...,./internal/jobidentity/...
COVERPROFILE := cover.out
COVER_MIN ?= 70

.PHONY: build
build:
	go build -o $(BINARY) ./cmd/firerunner

.PHONY: vet
vet:
	go vet $(PKG)

.PHONY: test
test:
	go test -race $(PKG)

# Unit coverage across all packages (cross-package attribution via -coverpkg).
.PHONY: cover
cover:
	go test -race -covermode=atomic -coverpkg=$(COVERPKG) -coverprofile=$(COVERPROFILE) $(PKG)
	go tool cover -func=$(COVERPROFILE) | tail -n 1

.PHONY: cover-html
cover-html: cover
	go tool cover -html=$(COVERPROFILE)

# Fail if total coverage is below COVER_MIN percent.
.PHONY: cover-check
cover-check: cover
	@total=$$(go tool cover -func=$(COVERPROFILE) | awk '/^total:/ {gsub(/%/,"",$$3); print $$3}'); \
	echo "total coverage: $$total% (min $(COVER_MIN)%)"; \
	awk -v t="$$total" -v m="$(COVER_MIN)" 'BEGIN { exit (t+0 < m+0) }' || \
		{ echo "coverage $$total% is below minimum $(COVER_MIN)%"; exit 1; }

# End-to-end tests require a real KVM host and are gated behind the `e2e` tag.
.PHONY: e2e
e2e:
	./test/e2e/run.sh

# The egress firewall against the real kernel: loads the generated nft ruleset
# into throwaway network namespaces and probes what a guest can reach. Needs
# Linux, root, ip and nft; the test binary is built as the user and run as root.
.PHONY: netns-test
netns-test:
	go test -c -tags netns -o egress-netns.test ./internal/provisioner/
	sudo env FR_REQUIRE_NETNS="$(FR_REQUIRE_NETNS)" ./egress-netns.test -test.run EgressNetNS -test.count 1 -test.v
	rm -f egress-netns.test

.PHONY: clean
clean:
	rm -f $(BINARY) $(COVERPROFILE) egress-netns.test
