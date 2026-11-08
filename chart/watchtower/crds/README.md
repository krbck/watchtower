# NOTE: helm applies the crds/ folder only on first install; upgrades skip
# it. When the CRD schema changes, apply manually via `make install-crd` or
# `kubectl apply -f config/crd/` (single source of truth: config/crd).
