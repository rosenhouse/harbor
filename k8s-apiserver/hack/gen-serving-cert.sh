#!/usr/bin/env bash
# Usage: gen-serving-cert.sh [namespace [apiservice]]
# Keeps a CA in Secret harbor-apiserver-ca and a serving certificate signed by it in Secret harbor-apiserver-tls.
# Issues a new serving certificate when it is missing, expires within 30 days, or has another issuer, but keeps the CA.
# Sets the APIService caBundle to the CA, or prints the caBundle if the APIService does not exist.
set -euo pipefail

namespace=${1:-harbor-apiserver}
apiservice=${2:-v1alpha1.harbor.goharbor.io}
host=harbor-apiserver.$namespace.svc

dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
cd "$dir"

secret_data() {
  kubectl -n "$namespace" get secret "$1" --ignore-not-found -o "go-template={{with index .data \"$2\"}}{{base64decode .}}{{end}}"
}

secret_data harbor-apiserver-ca tls.crt >ca.crt
secret_data harbor-apiserver-ca tls.key >ca.key
if [ ! -s ca.crt ] || [ ! -s ca.key ]; then
  openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -subj /CN=harbor-apiserver-ca -keyout ca.key -out ca.crt
  kubectl -n "$namespace" create secret tls harbor-apiserver-ca --cert=ca.crt --key=ca.key >&2
fi

secret_data harbor-apiserver-tls tls.crt >tls.crt
if ! openssl verify -CAfile ca.crt tls.crt >/dev/null 2>&1 || ! openssl x509 -in tls.crt -noout -checkend $((30 * 24 * 3600)) >/dev/null; then
  openssl req -new -newkey rsa:2048 -nodes -subj "/CN=$host" -keyout tls.key -out tls.csr
  printf 'subjectAltName=DNS:%s,DNS:%s.cluster.local\nbasicConstraints=critical,CA:FALSE\nextendedKeyUsage=serverAuth\n' "$host" "$host" >tls.ext
  openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 365 -extfile tls.ext -out tls.crt
  kubectl -n "$namespace" create secret generic harbor-apiserver-tls --type=kubernetes.io/tls \
    --from-file=tls.crt --from-file=tls.key --from-file=ca.crt --dry-run=client -o yaml | kubectl apply -f - >&2
fi

ca_bundle=$(base64 <ca.crt | tr -d '\n')
found=$(kubectl get apiservice "$apiservice" --ignore-not-found -o name)
if [ -n "$found" ]; then
  kubectl patch apiservice "$apiservice" --type=merge -p "{\"spec\":{\"caBundle\":\"$ca_bundle\",\"insecureSkipTLSVerify\":null}}" >&2
else
  echo "$ca_bundle"
fi
