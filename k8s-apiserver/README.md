# harbor-apiserver

A Kubernetes aggregated API server that serves a read-only view of Harbor repositories and artifacts.
The plan and design decisions are tracked in [issue 1](https://github.com/rosenhouse/harbor/issues/1).

## Development

```sh
go test ./...
hack/update-codegen.sh   # after changing pkg/apis
hack/e2e.sh              # needs docker, kind, kubectl, and helm
```

If the kind node cannot pull images, set `E2E_PRELOAD_IMAGES=1` so that `hack/e2e.sh` pulls Harbor's images on the host and loads them into kind.
Behind a TLS-intercepting proxy, set `E2E_CA_BUNDLE` to a CA bundle that the image build should trust instead of the system roots.
