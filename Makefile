# watchtower — development commands
# Go toolchain: go1.23+ (go.mod). Cluster: homelab k3s (kubeconfig: ~/.kube/config)

BINARY  := bin/manager
CRD     := config/crd/connectivitychecks.yaml
SAMPLE  := config/samples/connectivitycheck.yaml

.PHONY: build test vet run ui dev-up dev-down dev-status dev-logs install-crd uninstall-crd sample clean

build:
	go build -o $(BINARY) ./cmd/manager

test:
	go test ./...

vet:
	go vet ./...

# Out-of-cluster development mode: the CRD lives in the cluster, the manager
# runs locally. No need to build arm64 images for the homelab — the binary
# runs as x86 on the dev machine.
run:
	go run ./cmd/manager

# UI server: serves the embedded web UI on :8080 and proxies /api to the
# manager API (:8482). Run in a second terminal together with `make run`.
ui:
	go run ./cmd/ui

# Background supervisor (also auto-started by .vscode/tasks.json on folder open)
dev-up:
	bash scripts/dev.sh up

dev-down:
	bash scripts/dev.sh down

dev-status:
	bash scripts/dev.sh status

dev-logs:
	bash scripts/dev.sh logs

install-crd:
	kubectl apply -f $(CRD)

uninstall-crd:
	kubectl delete -f $(CRD) --ignore-not-found

sample:
	kubectl apply -f $(SAMPLE)

clean:
	rm -rf bin/
