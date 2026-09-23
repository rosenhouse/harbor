#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")/.."

hack/update-codegen.sh
untracked=$(git ls-files --others --exclude-standard -- pkg hack)
if ! git diff --exit-code -- pkg hack || [ -n "$untracked" ]; then
  echo "$untracked"
  echo "Generated code is stale. Run hack/update-codegen.sh." >&2
  exit 1
fi
