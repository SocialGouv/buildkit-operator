# buildcat — control plane for one hot vanilla buildkitd per (project, arch).

MODULE        := github.com/devthejo/buildcat
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.16.5
IMG_BUILDD    ?= ovh-registry/buildcat-buildd:dev
IMG_COMPANION ?= ovh-registry/buildcat-companion:dev

.PHONY: all
all: generate manifests build

## generate: deepcopy (zz_generated.deepcopy.go) for the API types.
.PHONY: generate
generate:
	$(CONTROLLER_GEN) object paths="./api/..."

## manifests: CRDs + RBAC from kubebuilder markers.
.PHONY: manifests
manifests:
	$(CONTROLLER_GEN) crd rbac:roleName=buildcat-buildd \
		paths="./api/...;./internal/..." \
		output:crd:artifacts:config=deploy/crd \
		output:rbac:artifacts:config=deploy/rbac
	cp deploy/crd/buildcat.dev_*.yaml deploy/helm/buildcat/crds/   # helm installs CRDs from chart crds/

## docker-build: build the buildd + companion images.
.PHONY: docker-build
docker-build:
	docker build -t $(IMG_BUILDD)    -f cmd/buildd/Dockerfile     .
	docker build -t $(IMG_COMPANION) -f cmd/companion/Dockerfile  .

.PHONY: docker-push
docker-push:
	docker push $(IMG_BUILDD)
	docker push $(IMG_COMPANION)

## build: compile all binaries into bin/.
.PHONY: build
build:
	go build -o bin/buildd     ./cmd/buildd
	go build -o bin/companion  ./cmd/companion
	go build -o bin/build      ./cmd/build

## test: unit tests.
.PHONY: test
test:
	go test ./... -count=1

## tidy: resolve module deps.
.PHONY: tidy
tidy:
	go mod tidy

## fmt/vet
.PHONY: fmt vet
fmt:; go fmt ./...
vet:; go vet ./...

## install: apply CRDs to the current kube context.
.PHONY: install
install: manifests
	kubectl apply -f deploy/crd

.PHONY: uninstall
uninstall:
	kubectl delete -f deploy/crd --ignore-not-found
