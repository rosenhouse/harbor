#!/usr/bin/env bash
# Deploys to a kind cluster and runs the e2e tests against it.
set -euo pipefail

cd "$(dirname "$0")/.."

cluster=${KIND_CLUSTER:-harbor-apiserver-e2e}
image=harbor-apiserver:e2e
export KUBECONFIG=${E2E_KUBECONFIG:-${TMPDIR:-/tmp}/$cluster.kubeconfig}

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --config hack/e2e/kind.yaml --wait 2m
fi
kind export kubeconfig --name "$cluster"

helm upgrade --install harbor harbor --repo https://helm.goharbor.io --version 1.19.2 \
  --namespace harbor --create-namespace --values hack/e2e/harbor-values.yaml --wait --timeout 10m

kubectl create namespace harbor-apiserver --dry-run=client -o yaml | kubectl apply -f -
go run -tags e2e ./test/e2e/seed

docker build -t "$image" .
kind load docker-image --name "$cluster" "$image"

existing=$(kubectl -n harbor-apiserver get deployment harbor-apiserver --ignore-not-found -o name)
kubectl apply -k hack/e2e
if [ -n "$existing" ]; then
  kubectl -n harbor-apiserver rollout restart deployment/harbor-apiserver
fi
kubectl -n harbor-apiserver rollout status deployment/harbor-apiserver --timeout=2m
kubectl wait --for=condition=Available apiservice/v1alpha1.harbor.goharbor.io --timeout=2m

go test -tags e2e -count=1 -v ./test/e2e/...
