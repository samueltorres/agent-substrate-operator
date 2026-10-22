CONTROLLER_GEN ?= go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.20.1
GOLANGCI_LINT ?= go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.14.0
ACTIONLINT ?= go run github.com/rhysd/actionlint/cmd/actionlint@v1.7.12
GOVULNCHECK ?= go run golang.org/x/vuln/cmd/govulncheck@v1.8.0
SHELLCHECK ?= $(shell command -v shellcheck 2>/dev/null || echo docker run --rm -v "$(CURDIR):/mnt" -w /mnt koalaman/shellcheck:v0.11.0@sha256:61862eba1fcf09a484ebcc6feea46f1782532571a34ed51fedf90dd25f925a8d)
CHART := charts/agent-substrate-operator
IMG ?= ghcr.io/samueltorres/agent-substrate-operator:dev

.PHONY: generate
generate: ## Regenerate deepcopy code, the CRD, and the operator's RBAC.
	$(CONTROLLER_GEN) object crd rbac:roleName=agent-substrate-operator paths=./... \
		output:crd:artifacts:config=$(CHART)/crds output:rbac:artifacts:config=config/rbac
	mkdir -p $(CHART)/files
	sed -n '/^rules:/,$$p' config/rbac/role.yaml | tail -n +2 > $(CHART)/files/clusterrole-rules.yaml

ENVTEST_K8S_VERSION ?= 1.37.0
ENVTEST ?= go run sigs.k8s.io/controller-runtime/tools/setup-envtest@release-0.24

.PHONY: test
test: ## Unit tests, and the controller tests against an envtest API server.
	KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(CURDIR)/bin/envtest -p path)" go test -race ./...

.PHONY: lint
lint:
	$(GOLANGCI_LINT) run ./...
	$(SHELLCHECK) hack/*.sh
	$(ACTIONLINT)
	helm lint $(CHART)
	helm lint $(CHART) --set substrateInstall.enabled=true

.PHONY: vulncheck
vulncheck:
	$(GOVULNCHECK) ./...

.PHONY: e2e
e2e: ## Install through the chart on a fresh kind cluster and run the counter demo; needs UPSTREAM.
	hack/e2e-kind.sh "$(UPSTREAM)"

.PHONY: verify
verify: generate lint test ## Fail if generated files or go.mod are stale.
	go mod tidy
	git diff --exit-code -- api $(CHART) config go.mod go.sum

.PHONY: build
build:
	go build -o bin/operator ./cmd/operator

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: run
run: ## Run the operator against the current kubeconfig context.
	go run ./cmd/operator --leader-elect=false
