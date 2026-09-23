#!/usr/bin/env bash
# Deploys to a kind cluster and runs the e2e tests against it.
set -euo pipefail

cd "$(dirname "$0")/.."

cluster=${KIND_CLUSTER:-harbor-apiserver-e2e}
image=harbor-apiserver:e2e

if ! kind get clusters | grep -qx "$cluster"; then
  kind create cluster --name "$cluster" --wait 2m
fi
kubectl config use-context "kind-$cluster"

docker build -t "$image" .
kind load docker-image --name "$cluster" "$image"

kubectl apply -k hack/e2e
kubectl -n harbor-apiserver rollout restart deployment/harbor-apiserver
kubectl -n harbor-apiserver rollout status deployment/harbor-apiserver --timeout=2m
kubectl wait --for=condition=Available apiservice/v1alpha1.harbor.goharbor.io --timeout=2m

go test -tags e2e -count=1 -v ./test/e2e/...
