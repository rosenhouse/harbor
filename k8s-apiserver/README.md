# harbor-apiserver

harbor-apiserver is a Kubernetes aggregated API server.
It serves a read-only view of one Harbor project's repositories and artifacts as the namespaced kinds `HarborRepository` and `HarborArtifact` in `harbor.goharbor.io/v1alpha1`.
It supports `get` and `list`, but not `watch`.
It reads Harbor as a project robot account on every request, and keeps no cache.
A namespace sees the project only when it has the label `harbor.goharbor.io/project=<project>`.

Kubernetes RBAC and that label are the only access controls.
Read the [threat model](docs/threat-model.md) before you install.
[Issue 1](https://github.com/rosenhouse/harbor/issues/1) tracks the plan and design decisions.

## Prerequisites

- A Kubernetes 1.25 or later cluster with the aggregation layer enabled, and cluster-admin access to it.
- Harbor 2.2 or later, reachable over HTTPS from the cluster's pods. The e2e tests use Harbor 2.15.2 from Helm chart 1.19.2.
- A Harbor project whose name is a valid label value (at most 63 characters).
- `git`, `kubectl`, `jq`, and Docker with Buildx.
- A registry that the cluster can pull from.
- [cert-manager](https://cert-manager.io) with its CA injector, or `openssl` for `hack/gen-serving-cert.sh`.

Run the commands below from `k8s-apiserver` in a clone of this repository:

```sh
git clone --depth 1 --branch agg https://github.com/rosenhouse/harbor.git
cd harbor/k8s-apiserver
```

## Build the image

No image is published.
Build and push your own, with `--platform` set to your nodes' architecture:

```sh
image=registry.example.com/harbor-apiserver:v0.1.0
docker buildx build --platform linux/amd64 -t "$image" --push .
```

The Dockerfile needs BuildKit, which `docker buildx` uses.

Don't host the image in the Harbor that it reads.
During a Harbor outage, new pods could not pull it, and an unavailable APIService breaks discovery and namespace deletion cluster-wide.

## Create a robot account

The server needs only these project permissions:

| Permission | Harbor API call |
| --- | --- |
| Repository: List | `GET /api/v2.0/projects/{project}/repositories` |
| Repository: Read | `GET /api/v2.0/projects/{project}/repositories/{repository}` |
| Artifact: List | `GET /api/v2.0/projects/{project}/repositories/{repository}/artifacts` |

Keep the robot's name and secret in a private directory until you create the Secret below:

```sh
dir=$(mktemp -d)
```

In the Harbor UI, open the project, then **Robot Accounts** > **New Robot Account**.
Name it `harbor-apiserver`, set an expiration, and select only the three permissions above.
Harbor then shows the robot's full name, such as `robot$my-project+harbor-apiserver`, and, only this once, its secret.
Save them in `$dir/username` and `$dir/password`.

Or, as a project admin, use the API:

```sh
curl -fsS -u my-harbor-admin -H 'Content-Type: application/json' \
  https://harbor.example.com/api/v2.0/robots -o "$dir/robot.json" -d '{
    "name": "harbor-apiserver",
    "level": "project",
    "duration": 90,
    "permissions": [{"kind": "project", "namespace": "my-project", "access": [
      {"resource": "repository", "action": "list"},
      {"resource": "repository", "action": "read"},
      {"resource": "artifact", "action": "list"}]}]}'
jq -r .name "$dir/robot.json" >"$dir/username"
jq -r .secret "$dir/robot.json" >"$dir/password"
```

`duration` is in days.

## Create the Secret

`url` must be Harbor's `https://` URL.
The server sends the robot's secret with every request, so an `http://` URL, such as that of Harbor's in-cluster Service, exposes it to anyone who can capture traffic on the pod network.

```sh
kubectl apply -f deploy/base/namespace.yaml
kubectl -n harbor-apiserver create secret generic harbor-apiserver \
  --from-literal=url=https://harbor.example.com \
  --from-literal=project=my-project \
  --from-file=username="$dir/username" \
  --from-file=password="$dir/password" \
  --dry-run=client -o yaml | kubectl apply --server-side -f -
rm -r "$dir"
```

If Harbor's certificate is not signed by a public CA, add `--from-file=ca.crt=harbor-ca.pem` to the command, and add the `--harbor-ca-file` patch to your overlay.

| Key | Used as | Read |
| --- | --- | --- |
| `url` | `--harbor-url` | at startup |
| `project` | `--harbor-project` | at startup |
| `username` | `--harbor-username-file=/etc/harbor/username` | on every Harbor request |
| `password` | `--harbor-password-file=/etc/harbor/password` | on every Harbor request |
| `ca.crt` (optional) | `--harbor-ca-file=/etc/harbor/ca.crt`, with the patch below | at startup |

To rotate the credentials before the robot expires, create a robot account with a new name, and rerun the command above with its files and the same other values.
Then restart the pods so that they read the new files, and delete the old robot account:

```sh
kubectl -n harbor-apiserver rollout restart deployment/harbor-apiserver
kubectl -n harbor-apiserver rollout status deployment/harbor-apiserver
```

Without a restart, the kubelet updates the mounted files within a minute or two.
Refreshing a robot's secret in Harbor invalidates the old secret at once, so requests fail until the pods read the new one.
After you change `url`, `project`, or `ca.crt`, restart the pods.

## Write an overlay

Create `my-install/kustomization.yaml`:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ../deploy/cert-manager   # or ../deploy, with hack/gen-serving-cert.sh
images:
  - name: harbor-apiserver
    newName: registry.example.com/harbor-apiserver
    newTag: v0.1.0
```

The options below each add an item to a `patches:` list in this file.
Keep a single `patches:` list: kustomize silently ignores all but the last of duplicate keys.

- If the Secret has `ca.crt`, pass it to the server:

  ```yaml
  - target: {kind: Deployment, name: harbor-apiserver}
    patch: |
      - op: add
        path: /spec/template/spec/containers/0/args/-
        value: --harbor-ca-file=/etc/harbor/ca.crt
  ```

- If the registry needs credentials, create a pull Secret in namespace `harbor-apiserver`, and name it here:

  ```yaml
  - target: {kind: ServiceAccount, name: harbor-apiserver}
    patch: |
      - op: add
        path: /imagePullSecrets
        value: [{name: my-pull-secret}]
  ```

The manifests assume the namespace `harbor-apiserver`.
Don't set kustomize's `namespace` field.
It moves the RoleBinding out of `kube-system`, and the server then fails to start (see the [threat model](docs/threat-model.md#changing-the-install-namespace)).

### Serving certificate

The APIService's `caBundle` lets kube-apiserver verify the server's certificate.
Pick one way to manage it:

- `../deploy/cert-manager` creates a self-signed CA in Secret `harbor-apiserver-ca` and a serving certificate in Secret `harbor-apiserver-tls`.
  cert-manager's CA injector copies the CA into the APIService, and cert-manager renews the certificate.
  Prefer this for long-lived clusters.
- `../deploy` with `hack/gen-serving-cert.sh` keeps a CA in Secret `harbor-apiserver-ca` and a serving certificate in Secret `harbor-apiserver-tls`, and sets the APIService's `caBundle`.
  The serving certificate lasts a year, and the script renews it when it expires within 30 days.
  Rerun the script at least monthly, as a user who can write Secrets in `harbor-apiserver` and patch APIServices.
  If the certificate expires, the APIService becomes unavailable, which breaks discovery and namespace deletion cluster-wide.

The server reloads a renewed certificate without a restart.

## Apply

```sh
kubectl apply -k my-install
hack/gen-serving-cert.sh   # only without cert-manager
kubectl -n harbor-apiserver rollout status deployment/harbor-apiserver
kubectl wait --for=condition=Available apiservice/v1alpha1.harbor.goharbor.io --timeout=2m
```

Without cert-manager, the APIService stays unavailable until the script runs.

## Verify

```console
$ kubectl get apiservice v1alpha1.harbor.goharbor.io
NAME                          SERVICE                             AVAILABLE   AGE
v1alpha1.harbor.goharbor.io   harbor-apiserver/harbor-apiserver   True        2m
$ kubectl api-resources --api-group=harbor.goharbor.io
NAME                 SHORTNAMES   APIVERSION                    NAMESPACED   KIND
harborartifacts                   harbor.goharbor.io/v1alpha1   true         HarborArtifact
harborrepositories                harbor.goharbor.io/v1alpha1   true         HarborRepository
```

## Label namespaces

```sh
kubectl label namespace my-namespace harbor.goharbor.io/project=my-project
```

In a namespace without this label, lists are empty and gets return NotFound.
To revoke access, remove the label:

```sh
kubectl label namespace my-namespace harbor.goharbor.io/project-
```

Anyone who can create namespaces or change their labels can grant visibility, and so can tools that create namespaces for tenants (see the [threat model](docs/threat-model.md#namespace-label-writers-grant-visibility)).
On Kubernetes 1.30 or later, this policy lets only the group `harbor-admins` set, change, or remove the label:

```yaml
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicy
metadata:
  name: harbor-project-label
spec:
  matchConstraints:
    resourceRules:
      - apiGroups: [""]
        apiVersions: [v1]
        operations: [CREATE, UPDATE]
        resources: [namespaces]
  variables:
    - name: project
      expression: "object.metadata.?labels[?'harbor.goharbor.io/project'].orValue('')"
    - name: oldProject
      expression: "oldObject == null ? '' : oldObject.metadata.?labels[?'harbor.goharbor.io/project'].orValue('')"
  validations:
    - expression: "variables.project == variables.oldProject || 'harbor-admins' in request.userInfo.groups"
      message: Only members of harbor-admins can set or change the label harbor.goharbor.io/project.
---
apiVersion: admissionregistration.k8s.io/v1
kind: ValidatingAdmissionPolicyBinding
metadata:
  name: harbor-project-label
spec:
  policyName: harbor-project-label
  validationActions: [Deny]
```

## Grant access

The ClusterRole `harbor.goharbor.io:view` grants `get` and `list` on both kinds.
It aggregates into the built-in `view` role, and so into `edit` and `admin`.
Everyone who can view a labeled namespace can read the whole project there.

For narrower access, add this item to the overlay's `patches:` list, which removes the aggregation label:

```yaml
- target: {kind: ClusterRole, name: "harbor.goharbor.io:view"}
  patch: |
    - op: remove
      path: /metadata/labels/rbac.authorization.k8s.io~1aggregate-to-view
```

Then bind the role explicitly:

```sh
kubectl -n my-namespace create rolebinding harbor-readers --clusterrole=harbor.goharbor.io:view --group=my-team
```

Listing across all namespaces needs cluster-wide `list`, and returns a copy of the project for each labeled namespace.

## Use

A `HarborRepository` is named after the repository within the project, with `/` replaced by `.`.
A repository name that contains a dot, or that is not a valid object name, gets a sanitized name with a hash suffix.

```console
$ kubectl -n my-namespace get harborrepositories -o wide
NAME                      REPOSITORY                ARTIFACTS   PULLS   AGE
app                       my-project/app            2           14      14d
team.api                  my-project/team/api       1           3       5d
web.frontend-bf9624cbaa   my-project/web.frontend   1           0       2h
```

A `HarborArtifact` is named after its repository and the first 12 hex digits of its digest.
Accessories, such as signatures, and the children of an index are not listed.

```console
$ kubectl -n my-namespace get harborartifacts
NAME                                          REPOSITORY                TAGS        TYPE    SIZE      AGE
app.sha256-c4710bc434ea                       my-project/app            <none>      IMAGE   3.4MiB    14d
app.sha256-f2524ca21741                       my-project/app            latest,v1   IMAGE   3.4MiB    12d
team.api.sha256-c78a0b5f2b06                  my-project/team/api       v1          IMAGE   21.7MiB   5d
web.frontend-bf9624cbaa.sha256-4b5e57f6eb2f   my-project/web.frontend   v1          IMAGE   8.0MiB    2h
```

Select one repository's artifacts by the `harbor.goharbor.io/repository` label, which holds the `HarborRepository` name, or by the `status.repository` field, which holds the full Harbor name.
Either selector makes the server read only that repository's artifacts from Harbor.

```console
$ kubectl -n my-namespace get harborartifacts -l harbor.goharbor.io/repository=team.api
NAME                           REPOSITORY            TAGS   TYPE    SIZE      AGE
team.api.sha256-c78a0b5f2b06   my-project/team/api   v1     IMAGE   21.7MiB   5d
$ kubectl -n my-namespace get harborartifacts --field-selector status.repository=my-project/team/api
NAME                           REPOSITORY            TAGS   TYPE    SIZE      AGE
team.api.sha256-c78a0b5f2b06   my-project/team/api   v1     IMAGE   21.7MiB   5d
$ kubectl -n my-namespace get harborartifact team.api.sha256-c78a0b5f2b06 -o jsonpath='{.status.digest}'
sha256:c78a0b5f2b067d7f1fce102453eac8a9f6a2a557ede231774cb293ed8b4c9308
```

The label is missing when the `HarborRepository` name is longer than 63 characters.
Select by field instead.

## Flags

These flags are specific to harbor-apiserver.
The Deployment sets all but `--harbor-ca-file`, `--harbor-timeout`, and `--kubeconfig`.

| Flag | Meaning |
| --- | --- |
| `--harbor-url` | URL of the Harbor server, such as `https://harbor.example.com`. Required. |
| `--harbor-project` | Harbor project to serve. It must be a valid label value. Required. |
| `--harbor-username-file` | File holding the robot account name. Required. |
| `--harbor-password-file` | File holding the robot account secret. Required. |
| `--harbor-ca-file` | PEM bundle of CAs to trust for Harbor, in addition to the system roots. |
| `--harbor-timeout` | Timeout for each HTTP request to Harbor. A list makes one request per page of 100. Default `10s`. |
| `--kubeconfig` | Kubeconfig for reading namespaces. Defaults to the in-cluster configuration. |

The other flags are the standard secure serving, delegated authentication and authorization, and logging flags of `k8s.io/apiserver`.
`docker run --rm "$image" --help` lists them all.
Keep `-v` below 8: from 8 up, client-go logs request bodies, including TokenReviews that hold callers' bearer tokens.

## Troubleshooting

Read the server's logs with `kubectl -n harbor-apiserver logs -l app=harbor-apiserver --prefix`.
When a request fails because of Harbor, the server logs the error as `"Harbor request failed"`.

### The APIService is not Available

`kubectl get apiservice v1alpha1.harbor.goharbor.io -o yaml` shows the reason in `status.conditions`.
While it is not Available, kubectl reports that it couldn't get the resource list for `harbor.goharbor.io/v1alpha1`, and namespaces cannot finish deleting.

- `MissingEndpoints`: no pod is ready. Run `kubectl -n harbor-apiserver describe pods`.
  - `ContainerCreating` means a Secret is missing. Check Secret `harbor-apiserver`, and Secret `harbor-apiserver-tls` from cert-manager (`kubectl -n harbor-apiserver get certificate`) or `hack/gen-serving-cert.sh`.
  - `CreateContainerConfigError` means Secret `harbor-apiserver` lacks `url` or `project`.
  - `ImagePullBackOff` or `ErrImagePull` means the nodes cannot pull the image. If the registry needs credentials, add the `imagePullSecrets` patch to your overlay.
  - `CrashLoopBackOff`: the logs name the problem, such as a missing flag, an invalid `--harbor-project`, a missing or empty credentials file, or an unreadable CA file.
    `exec format error` means the image was built for another architecture.
    `unable to load configmap based request-header-client-ca-file` means the RoleBinding in `kube-system` is missing.
  - `Running` but not ready means the server has not listed namespaces yet. Check the ClusterRoleBinding `harbor-apiserver`.
- `FailedDiscoveryCheck` with a certificate error: the `caBundle` does not match the serving certificate.
  Rerun `hack/gen-serving-cert.sh`, or check that cert-manager's CA injector is running.
- `FailedDiscoveryCheck` with a timeout or `dial tcp` error: kube-apiserver cannot reach the pods on TCP port 6443.
  Allow that traffic in any NetworkPolicy in `harbor-apiserver`, and in the firewall between the control plane and the nodes, as on GKE private clusters.

### 503 ServiceUnavailable: harbor is unavailable

The server could not reach Harbor, timed out waiting for Harbor to respond, or got a 429 or 5xx response.
A TLS error, such as an unknown certificate authority, also shows as unavailable; set `ca.crt` as above.
The APIService stays Available during a Harbor outage, by design.

A 503 that says "the server is currently unable to handle the request" comes from kube-apiserver instead, and means the APIService is not Available.

### 500 InternalError

- `harbor rejected the robot account credentials`: Harbor returned 401. The robot's name or secret is wrong, or the robot is expired or disabled.
- `harbor denied the robot account access`: Harbor returned 403. The project does not exist, the robot lacks a permission above, or the robot belongs to another project.
- `not found in harbor`: Harbor returned 404 when listing repositories. Check that `url` is Harbor's base URL, such as `https://harbor.example.com` without `/api/v2.0`, and that it reaches Harbor rather than another server.
- `unexpected error reading harbor`: see the logs.
  A timeout or dropped connection while the server reads Harbor's response causes this.
  So does a redirect, such as from `http://` to `https://`, because the server refuses redirects. Use Harbor's `https://` URL, which must not redirect.

### Broken credentials that still work

If the project is public, Harbor serves requests with a wrong, expired, or disabled robot's credentials anonymously, and the server keeps working.
harbor-core then logs an error, such as `failed to authenticate robot account` or `the robot account is expired`, for each request.
Check the robot's expiry in Harbor, or make the project private so that broken credentials fail.

## Uninstall

```sh
kubectl delete apiservice v1alpha1.harbor.goharbor.io
kubectl delete -k my-install --ignore-not-found
kubectl delete validatingadmissionpolicybinding,validatingadmissionpolicy harbor-project-label --ignore-not-found
kubectl label namespace -l harbor.goharbor.io/project harbor.goharbor.io/project-
```

Deleting the APIService first keeps discovery working while the pods stop.
Then delete the robot account in Harbor.

## Development

```sh
go test ./...
hack/update-codegen.sh   # after changing pkg/apis
hack/e2e.sh              # needs docker, kind, kubectl, helm, and openssl
```

If the kind node cannot pull images, set `E2E_PRELOAD_IMAGES=1` so that `hack/e2e.sh` pulls Harbor's images on the host and loads them into kind.
Behind a TLS-intercepting proxy, set `E2E_CA_BUNDLE` to a CA bundle that the image build should trust instead of the system roots.
