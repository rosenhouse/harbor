# harbor-apiserver

harbor-apiserver is a Kubernetes aggregated API server.
It serves a read-only view of one Harbor project's repositories and artifacts as the namespaced kinds `HarborRepository` and `HarborArtifact` in `harbor.goharbor.io/v1alpha1`.
It supports `get` and `list`, but not `watch`, because Harbor has no change feed.
So `kubectl get --watch`, informers, controller-runtime caches, and Argo CD don't work with these kinds, but `kubectl get` and other clients that only get and list do.
Each replica reads the project from Harbor as a project robot account every `--harbor-poll-interval`, and serves requests from that in-memory snapshot.
Replicas read Harbor independently, so two requests can see different snapshots.
Requests fail with 503 until a replica first reads Harbor successfully, and again once its snapshot is older than `--harbor-staleness-limit`.
A namespace sees the project only when it has the label `harbor.goharbor.io/project=<project>`.
Optionally, the server also lets users copy images into the project with the kind `HarborReplication` (see [Enable replications](#enable-replications)).

Kubernetes RBAC and that label are the only access controls.
Read the [threat model](docs/threat-model.md) before you install.
[Issue 1](https://github.com/rosenhouse/harbor/issues/1) tracks the plan and design decisions.

## Prerequisites

- A Kubernetes 1.25 or later cluster with the aggregation layer enabled, and cluster-admin access to it.
- Harbor 2.2 or later, reachable over HTTPS from the cluster's pods. The e2e tests use Helm chart 1.19.2 (Harbor 2.15.2), with core and jobservice from this fork.
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
| Artifact: List | `GET /api/v2.0/projects/{project}/repositories/{repository}/artifacts` |

Keep the robot's name and secret in a private directory until you create the Secret below:

```sh
dir=$(mktemp -d)
```

In the Harbor UI, open the project, then **Robot Accounts** > **New Robot Account**.
Name it `harbor-apiserver`, set an expiration, and select only the two permissions above.
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
      {"resource": "artifact", "action": "list"}]}]}'
jq -r .name "$dir/robot.json" >"$dir/username"
jq -r .secret "$dir/robot.json" >"$dir/password"
```

`duration` is in days.

## Create the Secret

`url` must be Harbor's `https://` URL.
The server sends the robot's secret with every request to Harbor, so an `http://` URL, such as that of Harbor's in-cluster Service, exposes it to anyone who can capture traffic on the pod network.

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
Refreshing a robot's secret in Harbor invalidates the old secret at once, so reads of Harbor fail until the pods read the new one.
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

## Enable replications

Replications let Kubernetes users copy images into the project from registry endpoints that you allow.
They are off by default, because they need a system-level Harbor robot account.
With Harbor from this fork, the robot can pull from any registry endpoint into the project, and start, stop, or delete any replication policy that pulls into it.
With upstream Harbor, it can replicate between any project and any endpoint.
Anyone who can read its Secret, or create pods in `harbor-apiserver`, can do the same.
Users who can create replications can copy anything that an allowed endpoint's credentials can read into the project, which every labeled namespace shares.
Read the [threat model](docs/threat-model.md#the-replication-robot-controls-pulls-into-the-project) before you enable them.

### Create registry endpoints

Users copy only from endpoints that a Harbor system administrator creates under **Administration** > **Registries** > **New Endpoint**, and that you allow in the Secret below.
Users name an endpoint, and never see its credentials.
Give every endpoint in Harbor credentials that can only read content that anyone who can reach Harbor may see, or none.
Never give an endpoint that points back at this Harbor the credentials of an account that can read private projects.

### Create a system robot account

Give the robot only these permissions:

| Scope | Resource | Actions |
| --- | --- | --- |
| System | Registry | List |
| Project | Replication Policy | List, Read, Create, Delete |
| Project | Replication | List, Create |

The server lists registry endpoints to find their IDs.
Only Harbor core from this fork's `agg` branch lets a system-level robot hold replication permissions on a project.
Run jobservice from the fork too, or a pull can mount any blob in Harbor that the source manifest names.

Harbor's UI can't grant the project permissions yet ([issue 31](https://github.com/rosenhouse/harbor/issues/31)), and editing the robot in the UI drops them.
So create the robot through the API, as a Harbor system administrator.
Keep its name and secret in a private directory until you create the Secret below:

```sh
dir=$(mktemp -d)
```

```sh
curl -fsS -u my-harbor-admin -H 'Content-Type: application/json' \
  https://harbor.example.com/api/v2.0/robots -o "$dir/robot.json" -d '{
    "name": "harbor-apiserver-replication",
    "level": "system",
    "duration": 90,
    "permissions": [
      {"kind": "system", "namespace": "/", "access": [
        {"resource": "registry", "action": "list"}]},
      {"kind": "project", "namespace": "my-project", "access": [
        {"resource": "replication-policy", "action": "list"},
        {"resource": "replication-policy", "action": "read"},
        {"resource": "replication-policy", "action": "create"},
        {"resource": "replication-policy", "action": "delete"},
        {"resource": "replication", "action": "list"},
        {"resource": "replication", "action": "create"}]}]}'
jq -r .name "$dir/robot.json" >"$dir/username"
jq -r .secret "$dir/robot.json" >"$dir/password"
```

Upstream Harbor accepts replication permissions only at the system level, so move the project entry's actions into the system entry.

### Create the replication Secret

`registries` holds the names of the endpoints that replications may copy from, separated by commas.

```sh
kubectl -n harbor-apiserver create secret generic harbor-apiserver-replication \
  --from-literal=registries=docker-hub,quay \
  --from-file=username="$dir/username" \
  --from-file=password="$dir/password" \
  --dry-run=client -o yaml | kubectl apply --server-side -f -
rm -r "$dir"
```

| Key | Used as | Read |
| --- | --- | --- |
| `registries` | `--replication-registries` | at startup |
| `username` | `--replication-username-file=/etc/harbor-replication/username` | on every Harbor request |
| `password` | `--replication-password-file=/etc/harbor-replication/password` | on every Harbor request |

The server reaches Harbor at the same `url`, with the same `ca.crt`, as for the project robot.
Rotate these credentials as the project robot's, and restart the pods after you change `registries`.

### Add the component

Add the component to `my-install/kustomization.yaml`:

```yaml
components:
  - ../deploy/components/replication
```

Then apply it:

```sh
kubectl apply -k my-install
kubectl -n harbor-apiserver rollout status deployment/harbor-apiserver
```

`kubectl api-resources --api-group=harbor.goharbor.io` then lists `harborreplications`.

Each replication copies into the project under `--replication-prefix`, which defaults to `k8s`.
If another cluster replicates into the same project, give each cluster its own prefix with this item in the overlay's `patches:` list:

```yaml
- target: {kind: Deployment, name: harbor-apiserver}
  patch: |
    - op: add
      path: /spec/template/spec/containers/0/args/-
      value: --replication-prefix=k8s-prod
```

Changing the prefix hides existing replications, so set it before you create any.

### Grant replication access

The component adds two ClusterRoles:

- `harbor.goharbor.io:view-replications` grants `get` and `list` on replications, and aggregates into `view`, like `harbor.goharbor.io:view`.
- `harbor.goharbor.io:replicate` grants `create`, `update`, `patch`, and `delete`. It does not aggregate, so bind it explicitly:

  ```sh
  kubectl -n my-namespace create rolebinding harbor-replicators --clusterrole=harbor.goharbor.io:replicate --group=my-team
  ```

`kubectl apply` also needs `get`, which `view` grants.
To narrow who can view replications, remove the aggregation label of `harbor.goharbor.io:view-replications` as in [Grant access](#grant-access).

### Replicate

```sh
kubectl apply -f - <<'EOF'
apiVersion: harbor.goharbor.io/v1alpha1
kind: HarborReplication
metadata:
  name: nginx
  namespace: my-namespace
spec:
  registry: docker-hub
  repository: library/nginx
  tag: "1.27.5"
  schedule: "0 0 3 * * *"
EOF
```

- `registry` names an allowed endpoint.
- `repository` is one source repository, without a glob.
- `tag` is Harbor's tag filter, a pattern such as `1.27.*`. `*` copies every tag.
- `schedule` is optional. Here, it reruns the replication daily at 03:00 UTC.

The replication runs once when you create it.
Poll with `kubectl get` until its phase is no longer `InProgress`:

```console
$ kubectl -n my-namespace get harborreplications
NAME    REGISTRY     REPOSITORY      TAG      SCHEDULE      PHASE       AGE
nginx   docker-hub   library/nginx   1.27.5   0 0 3 * * *   Succeeded   2m
$ kubectl -n my-namespace get harborreplication nginx -o jsonpath='{.status.destination}'
my-project/k8s/my-namespace/nginx
```

The phase is the last run's: `InProgress`, `Succeeded`, `Failed`, or `Stopped`.
A run that Harbor skips, because the previous run is still going, does not count.
A run that copies nothing, such as when the repository does not exist or no tag matches, is `Succeeded` with `total` 0 and the message `no resources need to be replicated`.
`status.lastExecution` also holds the run's counts and Harbor's message.
That message can hold endpoint URLs and the source registry's responses, and anyone who can view the replication can read it.
A source repository keeps its path under `status.destination`.

In the replication's namespace, the copied artifacts have the label `harbor.goharbor.io/replication` and an ownerReference to the replication:

```console
$ kubectl -n my-namespace get harborartifacts -l harbor.goharbor.io/replication=nginx
NAME                                                       REPOSITORY                                        TAGS     TYPE    SIZE      AGE
k8s.my-namespace.nginx.library.nginx.sha256-6784fb0834aa   my-project/k8s/my-namespace/nginx/library/nginx   1.27.5   IMAGE   68.9MiB   2m
```

Other labeled namespaces see these artifacts too, but without the label or the ownerReference.
The ownerReference only shows where the artifacts came from. Deleting the replication leaves them.

### Limits

- A replication cannot change, not even its labels and annotations.
  An update, patch, or server-side apply succeeds only if it changes nothing, and otherwise fails with `field is immutable`.
  A server-side apply also fails if its manifest lacks a field or label of the replication, because the server keeps no managed fields.
  Without `--force-conflicts`, it reports a changed value as a conflict with `before-first-apply` instead.
  To change a replication, delete and recreate it, such as with `kubectl apply --force`, or Flux's `spec.force: true`.
  Leave labels that change with each chart version, such as `helm.sh/chart`, off replications, or `helm upgrade` fails.
- The server ignores changes to the annotation `kubectl.kubernetes.io/last-applied-configuration`.
  So client-side `kubectl apply` of a replication that it did not create warns that the annotation is missing, and reports `configured` without a change.
- There is no watch. `kubectl get -w` fails, and `kubectl wait` logs watch errors and notices changes late.
- Argo CD applies replications, but never prunes them or deletes them with their Application, because it tracks only kinds that it can watch. Delete them yourself.
  Its Server-Side Diff fails on replications, so give such Applications the annotation `argocd.argoproj.io/compare-options: ServerSideDiff=false`.
- The status has no conditions. So health checks that read conditions, such as Flux's `wait`, count a replication as ready whatever its last run did.
- The server does not keep `generateName`.
- There is no way to rerun a replication on demand yet.
- Deleting a replication stops its runs, and deletes its policy and run history from Harbor. It leaves the copied artifacts.
  If the runs have not stopped after 30 seconds, the delete fails with a Conflict. Retry it.
- Deleting a labeled namespace deletes its replications' policies, and leaves their artifacts.
  The namespace stays Terminating until Harbor answers.
- Removing a namespace's label, an endpoint from `registries`, or the component hides the affected replications, but their schedules keep running in Harbor.
  Delete the replications first.
- A policy that the server hides blocks its namespace and name until a Harbor administrator deletes it.
  Examples are a policy from an earlier namespace of the same name, one that someone changed in Harbor, and one that pulls into another project (see the [threat model](docs/threat-model.md#removing-the-label-leaves-replications-running)).
  Creating the replication fails with AlreadyExists, and the message names the policy.
- The server needs Harbor 2.3 or later. Before Harbor 2.14, a run can start while the previous one is still running.
- A schedule has 6 fields separated by single spaces: seconds, minutes, hours, day of month, month, and day of week, in UTC.
  Seconds must be `0`, and minutes a single number, so a replication runs at most once an hour.
  It must be at most 64 characters, and match a date that exists.
- A tag pattern holds only letters, digits, and the characters `_.-*?[]^`, with at most two `*`, so that Harbor matches each tag in milliseconds.
  It cannot hold `{}` alternatives. Create a replication for each alternative instead.
- The destination repository, `<project>/<prefix>/<namespace>/<name>/<repository>`, must fit in 255 characters.
- A replication's labels and annotations together must fit in 8192 bytes, because its Harbor policy holds them.
  Client-side `kubectl apply` adds an annotation that holds the whole manifest, so apply a large manifest with `kubectl create` or `--server-side`.

## Flags

These flags are specific to harbor-apiserver.
The Deployment sets only the required ones.

| Flag | Meaning |
| --- | --- |
| `--harbor-url` | URL of the Harbor server, such as `https://harbor.example.com`. Required. |
| `--harbor-project` | Harbor project to serve. It must be a valid label value. Required. |
| `--harbor-username-file` | File holding the robot account name. Required. |
| `--harbor-password-file` | File holding the robot account secret. Required. |
| `--harbor-ca-file` | PEM bundle of CAs to trust for Harbor, in addition to the system roots. |
| `--harbor-timeout` | Timeout for each HTTP request to Harbor. It must be positive. A list makes one request per page of 100. Default `10s`. |
| `--harbor-poll-interval` | How long to wait between polls of the project. Each wait adds up to 10% jitter. After a poll fails, the replica retries once after 1 second, and then waits this interval until a poll succeeds. Default `30s`. |
| `--harbor-staleness-limit` | How old data can be before requests that need it fail with 503. A poll that takes longer than this fails. It must be longer than 2.2 times `--harbor-poll-interval` plus twice `--harbor-timeout`. Default `5m`. |
| `--kubeconfig` | Kubeconfig for reading namespaces. Defaults to the in-cluster configuration. |
| `--enable-replications` | Serve the writable `HarborReplication` kind. It needs a system-level Harbor robot account (see the [threat model](docs/threat-model.md#the-replication-robot-controls-pulls-into-the-project)). The other `--replication-*` flags need it. Default `false`. |
| `--replication-registries` | Names of the Harbor registry endpoints that replications may copy from, separated by commas. Required with `--enable-replications`. |
| `--replication-username-file` | File holding the replication robot account name. Required with `--enable-replications`. |
| `--replication-password-file` | File holding the replication robot account secret. Required with `--enable-replications`. |
| `--replication-prefix` | Path segment in the project that replications copy into, and the prefix of their Harbor policy names. It must be a DNS label. Clusters that share a project need different prefixes. Default `k8s`. |

The other flags are the standard secure serving, delegated authentication and authorization, and logging flags of `k8s.io/apiserver`.
`docker run --rm "$image" --help` lists them all.
Keep `-v` below 8: from 8 up, client-go logs request bodies, including TokenReviews that hold callers' bearer tokens.

## Troubleshooting

Read the server's logs with `kubectl -n harbor-apiserver logs -l app=harbor-apiserver --prefix`.
When a poll of Harbor fails, the server logs the error as `"Reading Harbor failed"`.
A poll that takes longer than `--harbor-poll-interval` logs `"Reading Harbor took longer than the poll interval"`.
When one repository's artifact list fails without failing the poll, the server logs `"Listing a repository's artifacts failed, so the server keeps those it listed before"`.
Once that repository's artifacts are older than `--harbor-staleness-limit`, each poll logs `"Requests for a repository's artifacts fail, because they were not listed within the staleness limit"`.
Both messages name the repository.
The [threat model](docs/threat-model.md#harbor-outages) explains which requests fail.

### The APIService is not Available

`kubectl get apiservice v1alpha1.harbor.goharbor.io -o yaml` shows the reason in `status.conditions`.
While it is not Available, kubectl reports that it couldn't get the resource list for `harbor.goharbor.io/v1alpha1`, and namespaces cannot finish deleting.

- `MissingEndpoints`: no pod is ready. Run `kubectl -n harbor-apiserver describe pods`.
  - `ContainerCreating` means a Secret is missing. Check Secret `harbor-apiserver`, Secret `harbor-apiserver-tls` from cert-manager (`kubectl -n harbor-apiserver get certificate`) or `hack/gen-serving-cert.sh`, and, with replications, Secret `harbor-apiserver-replication`.
  - `CreateContainerConfigError` means Secret `harbor-apiserver` lacks `url` or `project`, or Secret `harbor-apiserver-replication` lacks `registries`.
  - `ImagePullBackOff` or `ErrImagePull` means the nodes cannot pull the image. If the registry needs credentials, add the `imagePullSecrets` patch to your overlay.
  - `CrashLoopBackOff`: the logs name the problem, such as a missing flag, an invalid `--harbor-project`, a `--harbor-staleness-limit` that is too short, a missing or empty credentials file, or an unreadable CA file.
    `exec format error` means the image was built for another architecture.
    `unable to load configmap based request-header-client-ca-file` means the RoleBinding in `kube-system` is missing.
  - `Running` but not ready means the server has not listed namespaces yet, which needs the ClusterRoleBinding `harbor-apiserver`, or its first poll of Harbor has not ended. `--harbor-staleness-limit` bounds that poll.
- `FailedDiscoveryCheck` with a certificate error: the `caBundle` does not match the serving certificate.
  Rerun `hack/gen-serving-cert.sh`, or check that cert-manager's CA injector is running.
- `FailedDiscoveryCheck` with a timeout or `dial tcp` error: kube-apiserver cannot reach the pods on TCP port 6443.
  Allow that traffic in any NetworkPolicy in `harbor-apiserver`, and in the firewall between the control plane and the nodes, as on GKE private clusters.

### 503 ServiceUnavailable

A 503's message gives the reason:

- `harbor has not been read yet`: the replica's first poll has not ended.
- `harbor is unavailable`: the server could not reach Harbor, timed out waiting for Harbor to respond, or got a 429 or 5xx response.
  A TLS error, such as an unknown certificate authority, also shows as unavailable; set `ca.crt` as above.
- `reading harbor takes longer than the staleness limit`: a poll ran longer than `--harbor-staleness-limit`, which cancelled it, or the data passed the limit while a poll was running.
  Harbor is slow, or the project is too large for the limit. Raise `--harbor-staleness-limit`.
- `reading a repository's artifacts failed`: the response could include artifacts of a repository that the server has not listed within `--harbor-staleness-limit`. The logs name the repository.
- `harbor rejected the robot account credentials`: Harbor returned 401. The robot's name or secret is wrong, or the robot is expired or disabled.
- `harbor denied the robot account access`: Harbor returned 403. The project does not exist, the robot lacks a permission above, or the robot belongs to another project.
- `not found in harbor`: Harbor returned 404 when listing repositories. Check that `url` is Harbor's base URL, such as `https://harbor.example.com` without `/api/v2.0`, and that it reaches Harbor rather than another server.
- `unexpected error reading harbor`: see the logs.
  A timeout or dropped connection while the server reads the repository list's response causes this.
  So does a redirect, such as from `http://` to `https://`, because the server refuses redirects. Use Harbor's `https://` URL, which must not redirect.

The APIService stays Available during a Harbor outage, by design.

A 503 that says "the server is currently unable to handle the request" comes from kube-apiserver instead, and means the APIService is not Available.

### Broken credentials that still work

If the project is public, Harbor serves requests with a wrong, expired, or disabled robot's credentials anonymously, and the server keeps working.
harbor-core then logs an error, such as `failed to authenticate robot account` or `the robot account is expired`, for each request.
Check the robot's expiry in Harbor, or make the project private so that broken credentials fail.

### Replications fail

Requests for replications call Harbor with the replication robot, and report its failures with these messages:

- `harbor is unavailable` (503), as above.
- `harbor rejected the robot account credentials` (500): the replication robot's name or secret is wrong, or the robot is expired or disabled.
- `harbor denied the robot account access` (500): the replication robot lacks a permission above.
- `harbor rejected the replication policy` (400): the logs give Harbor's reason.
- `unexpected error from harbor` (500): see the logs.

A create in a namespace that does not exist, or lacks the label, fails with Forbidden.
`spec.registry: Unsupported value` means that the endpoint is not in `registries`, and `spec.registry: Not found` means that Harbor has no endpoint with that name.

If a create fails after Harbor stored the policy, such as when Harbor fails to start the first run, the server deletes the policy, so that a retry creates it again.
If that delete fails too, the server logs `"Deleting the policy of a failed create failed, so the replication may exist without a run"`. Delete and recreate the replication.
`status.lastExecution.message` says why a run failed, and Harbor shows each task's log under **Administration** > **Replications**.

When a poll cannot list the replication policies, the server logs `"Listing replication policies failed, so artifacts keep their links from the last list that succeeded"`.
Artifacts keep their labels and ownerReferences from that list.
Once that list is older than `--harbor-staleness-limit`, lists of artifacts by the `harbor.goharbor.io/replication` label fail with a 503 that starts with `listing replication policies failed`.

## Uninstall

If replications are enabled, delete them first, or their policies stay in Harbor, and their schedules keep running:

```sh
kubectl delete harborreplications --all --all-namespaces
```

Then remove the rest:

```sh
kubectl delete apiservice v1alpha1.harbor.goharbor.io
kubectl delete -k my-install --ignore-not-found
kubectl delete validatingadmissionpolicybinding,validatingadmissionpolicy harbor-project-label --ignore-not-found
kubectl label namespace -l harbor.goharbor.io/project harbor.goharbor.io/project-
```

Deleting the APIService first keeps discovery working while the pods stop.
Then delete the robot accounts in Harbor.

## Development

```sh
go test ./...
hack/update-codegen.sh   # after changing pkg/apis
hack/e2e.sh              # needs docker, kind, kubectl, helm, and openssl
```

If the kind node cannot pull images, set `E2E_PRELOAD_IMAGES=1` so that `hack/e2e.sh` pulls Harbor's images on the host and loads them into kind.
Behind a TLS-intercepting proxy, set `E2E_CA_BUNDLE` to a CA bundle that the image build should trust instead of the system roots.
