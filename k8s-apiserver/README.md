# harbor-apiserver

A Kubernetes aggregated API server that serves a read-only view of Harbor repositories and artifacts.
The plan and design decisions are tracked in [issue 1](https://github.com/rosenhouse/harbor/issues/1).

## Development

```sh
go test ./...
hack/update-codegen.sh   # after changing pkg/apis
```
