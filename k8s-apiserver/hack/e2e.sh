#!/usr/bin/env bash
# Deploys to a kind cluster and runs the e2e tests against it.
set -euo pipefail

ca_bundle=${E2E_CA_BUNDLE:+$(realpath "$E2E_CA_BUNDLE")}
cd "$(dirname "$0")/.."

cluster=${KIND_CLUSTER:-harbor-apiserver-e2e}
image=harbor-apiserver:e2e
harbor_version=1.19.2
harbor_chart=(harbor --repo https://helm.goharbor.io --version "$harbor_version" --namespace harbor --values hack/e2e/harbor-values.yaml)
export KUBECONFIG=${E2E_KUBECONFIG:-${TMPDIR:-/tmp}/$cluster.kubeconfig}

if ! kind get clusters | grep -x "$cluster" >/dev/null; then
  kind create cluster --name "$cluster" --config hack/e2e/kind.yaml --wait 2m
fi
if [ "$(docker inspect -f '{{range index .HostConfig.PortBindings "30002/tcp"}}{{.HostPort}}{{end}}' "$cluster-control-plane")" != 30002 ]; then
  echo "kind cluster $cluster does not map Harbor's NodePort 30002 to the host. Delete it with: kind delete cluster --name $cluster" >&2
  exit 1
fi
kind export kubeconfig --name "$cluster"

build_log=$(mktemp)
docker build ${ca_bundle:+--secret "id=ca-bundle,src=$ca_bundle"} -t "$image" . >"$build_log" 2>&1 &
build=$!
trap 'kill "$build" 2>/dev/null || true; rm -f "$build_log"' EXIT

# An interrupted TestHarborOutageIsServiceUnavailable leaves Harbor scaled down.
if kubectl -n harbor get deployment harbor-nginx >/dev/null 2>&1; then
  kubectl -n harbor scale deployment/harbor-nginx --replicas=1
fi

# Every upgrade regenerates Harbor's token CA and restarts it, so upgrade only when the chart or values change.
harbor_release="harbor-$harbor_version values $(cksum <hack/e2e/harbor-values.yaml)"
if ! helm history harbor --namespace harbor --max 1 -o json 2>/dev/null | grep -F "\"description\":\"$harbor_release\"" >/dev/null; then
  if [ "${E2E_PRELOAD_IMAGES:-}" = 1 ]; then
    images=$(helm template harbor "${harbor_chart[@]}" | sed -nE 's/^[ -]*image: *"?([^"]+)"?$/\1/p' | sort -u)
    for i in $images; do
      docker image inspect "$i" >/dev/null 2>&1 ||
        { docker pull "mirror.gcr.io/${i#docker.io/}" && docker tag "mirror.gcr.io/${i#docker.io/}" "$i"; } ||
        docker pull "$i"
    done
    # Docker pulls only the host's platform, and kind cannot import an archive with missing platforms.
    docker save --platform "linux/$(docker version -f '{{.Server.Arch}}')" $images | kind load image-archive --name "$cluster" /dev/stdin
  fi
  helm upgrade --install harbor "${harbor_chart[@]}" --description "$harbor_release" --create-namespace --wait --timeout 10m
fi
kubectl -n harbor wait --for=condition=Available deployment --all --timeout=5m
kubectl -n harbor rollout status statefulset --timeout=5m

kubectl create namespace harbor-apiserver --dry-run=client -o yaml | kubectl apply -f -
go run ./test/e2e/seed

wait "$build" || { cat "$build_log" >&2; exit 1; }
docker save "$image" | kind load image-archive --name "$cluster" /dev/stdin

existing=$(kubectl -n harbor-apiserver get deployment harbor-apiserver --ignore-not-found -o name)
kubectl apply -k hack/e2e
hack/gen-serving-cert.sh
if [ -n "$existing" ]; then
  kubectl -n harbor-apiserver rollout restart deployment/harbor-apiserver
fi
kubectl -n harbor-apiserver rollout status deployment/harbor-apiserver --timeout=2m
kubectl wait --for=condition=Available apiservice/v1alpha1.harbor.goharbor.io --timeout=2m

go test -tags e2e -count=1 -timeout 20m -v ./test/e2e/...
