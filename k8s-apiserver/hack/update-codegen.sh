#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

module=$(go list -m)
header=hack/boilerplate.go.txt
apis=./pkg/apis/harbor/v1alpha1

go tool deepcopy-gen --output-file zz_generated.deepcopy.go --go-header-file "$header" "$apis"

go tool openapi-gen \
  --output-file zz_generated.openapi.go \
  --output-dir pkg/generated/openapi \
  --output-pkg "$module/pkg/generated/openapi" \
  --go-header-file "$header" \
  --report-filename hack/api-rule-violations.list \
  --output-model-name-file zz_generated.model_name.go \
  k8s.io/apimachinery/pkg/apis/meta/v1 \
  k8s.io/apimachinery/pkg/runtime \
  k8s.io/apimachinery/pkg/version \
  "$apis"
