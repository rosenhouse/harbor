#!/usr/bin/env bash
# Deploys to a kind cluster and runs the e2e tests against it.
set -euo pipefail

cd "$(dirname "$0")/.."

cluster=${KIND_CLUSTER:-harbor-apiserver-e2e}
image=harbor-apiserver:e2e
harbor_chart=(harbor --repo https://helm.goharbor.io --version 1.19.2 --namespace harbor --values hack/e2e/harbor-values.yaml)
export KUBECONFIG=${E2E_KUBECONFIG:-${TMPDIR:-/tmp}/$cluster.kubeconfig}

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --config hack/e2e/kind.yaml --wait 2m
fi
if ! docker port "$cluster-control-plane" 30002/tcp >/dev/null 2>&1; then
  echo "kind cluster $cluster does not map Harbor's NodePort 30002 to the host. Delete it with: kind delete cluster --name $cluster" >&2
  exit 1
fi
kind export kubeconfig --name "$cluster"

docker build ${E2E_CA_BUNDLE:+--secret "id=ca-bundle,src=$E2E_CA_BUNDLE"} -t "$image" . &
build=$!

# Every upgrade regenerates Harbor's token CA and restarts it, so install only once.
if ! helm status harbor --namespace harbor 2>/dev/null | grep -x 'STATUS: deployed' >/dev/null; then
  if [ "${E2E_PRELOAD_IMAGES:-}" = 1 ]; then
    images=$(helm template harbor "${harbor_chart[@]}" | sed -nE 's/^[ -]*image: *"?([^"]+)"?$/\1/p' | sort -u)
    for i in $images; do
      docker image inspect "$i" >/dev/null 2>&1 ||
        { docker pull "mirror.gcr.io/${i#docker.io/}" && docker tag "mirror.gcr.io/${i#docker.io/}" "$i"; } ||
        docker pull "$i"
    done
    docker save $images | kind load image-archive --name "$cluster" /dev/stdin
  fi
  helm upgrade --install harbor "${harbor_chart[@]}" --create-namespace --wait --timeout 10m
fi
kubectl -n harbor wait --for=condition=Available deployment --all --timeout=5m

kubectl create namespace harbor-apiserver --dry-run=client -o yaml | kubectl apply -f -
go run -tags e2e ./test/e2e/seed

wait "$build"
docker save "$image" | kind load image-archive --name "$cluster" /dev/stdin

existing=$(kubectl -n harbor-apiserver get deployment harbor-apiserver --ignore-not-found -o name)
kubectl apply -k hack/e2e
if [ -n "$existing" ]; then
  kubectl -n harbor-apiserver rollout restart deployment/harbor-apiserver
fi
kubectl -n harbor-apiserver rollout status deployment/harbor-apiserver --timeout=2m
kubectl wait --for=condition=Available apiservice/v1alpha1.harbor.goharbor.io --timeout=2m

go test -tags e2e -count=1 -v ./test/e2e/...
