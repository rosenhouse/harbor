# harbor-apiserver

A Kubernetes aggregated API server that serves a read-only view of Harbor repositories and artifacts.
The plan and design decisions are tracked in [issue 1](https://github.com/rosenhouse/harbor/issues/1).

## Install

No image is published yet, so build and push your own.
The server reads one Harbor project as a project robot account that can list and read repositories and artifacts.
Until [issue 11](https://github.com/rosenhouse/harbor/issues/11), the APIService skips TLS verification of the server.

```sh
image=registry.example.com/harbor-apiserver:dev
docker build -t "$image" . && docker push "$image"
(cd deploy && kustomize edit set image harbor-apiserver="$image")

kubectl apply -f deploy/namespace.yaml
kubectl -n harbor-apiserver create secret generic harbor-apiserver \
  --from-literal=url=https://harbor.example.com \
  --from-literal=project=my-project \
  --from-literal=username='robot$my-project+k8s' \
  --from-literal=password="$ROBOT_SECRET"
kubectl apply -k deploy
kubectl wait --for=condition=Available apiservice/v1alpha1.harbor.goharbor.io
kubectl label namespace my-namespace harbor.goharbor.io/project=my-project
```

## Development

```sh
go test ./...
hack/update-codegen.sh   # after changing pkg/apis
hack/e2e.sh              # needs docker, kind, kubectl, and helm
```

If the kind node cannot pull images, set `E2E_PRELOAD_IMAGES=1` so that `hack/e2e.sh` pulls Harbor's images on the host and loads them into kind.
Behind a TLS-intercepting proxy, set `E2E_CA_BUNDLE` to a CA bundle that the image build should trust instead of the system roots.
