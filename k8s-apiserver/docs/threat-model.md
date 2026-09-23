# Threat model

harbor-apiserver shows one Harbor project's metadata to Kubernetes users.
This model covers an install from `deploy/` as the [README](../README.md) describes.

## Assets

- Secret `harbor-apiserver` holds the robot credentials.
  With the README's permissions, they can list repository and artifact metadata in one project, but cannot pull, push, or change anything.
- The project metadata includes repository names and descriptions, artifact digests, tags, sizes, media types, OCI annotations, and push and pull times and counts.
- Secret `harbor-apiserver-ca` holds the serving CA's key, and Secret `harbor-apiserver-tls` holds the serving key.
  The aggregator trusts every serving certificate that the CA signs for this API.
- The cluster's API depends on this server, because an unavailable APIService breaks discovery and namespace deletion cluster-wide.
- Harbor's availability is at stake, because each replica reads the whole project from Harbor every poll interval.

## Actors

- **Namespace users** can get or list the two kinds in a namespace, usually through the `view`, `edit`, or `admin` role.
- **Namespace-label writers** can create namespaces or change their labels, directly or through a tool.
- **Cluster admins** can read Secrets and change RBAC, the APIService, and the `harbor-apiserver` namespace.
- **Harbor admins** manage the project, its visibility, and the robot account.
- **Project pushers** control repository names, digests, tags, and annotations.
- **Network attackers** can reach the pod network or the path from the pods to Harbor.

## Trust boundaries

1. **Client to kube-apiserver.**
   kube-apiserver authenticates and authorizes the user, and writes the Kubernetes audit log if the cluster has an audit policy.
   It removes the user's `Authorization` header before it proxies the request.
2. **kube-apiserver to harbor-apiserver.**
   The aggregator verifies the serving certificate against the APIService's `caBundle` for `harbor-apiserver.harbor-apiserver.svc`.
   It authenticates with its front-proxy client certificate and names the user in `X-Remote-*` headers.
   harbor-apiserver authorizes each request again with a SubjectAccessReview, except for members of `system:masters`.
3. **Pods to harbor-apiserver.**
   The manifests have no NetworkPolicy, so any pod can call the Service directly.
   harbor-apiserver accepts client certificates signed by the cluster's client CA, checks bearer tokens with a TokenReview, and treats requests without credentials as `system:anonymous`.
   It authorizes them as in boundary 2.
   `/healthz`, `/readyz`, and `/livez` skip authorization.
4. **harbor-apiserver to Harbor.**
   Every request carries the robot's credentials as HTTP basic auth.
5. **harbor-apiserver to kube-apiserver.**
   The ServiceAccount can list and watch namespaces, create TokenReviews and SubjectAccessReviews, and read ConfigMap `kube-system/extension-apiserver-authentication` ([rbac.yaml](../deploy/base/rbac.yaml)).

## Threats

### Every authorized user sees what the robot sees

The server does not filter by user or by repository.
Anyone allowed to list either kind in a labeled namespace sees every repository and artifact that the robot can list.
`harbor.goharbor.io:view` aggregates into `view`, so this includes every user and ServiceAccount bound to `view`, `edit`, or `admin` there.
Harbor project membership does not apply to Kubernetes users.

Mitigations:

- The robot reads one project, with the two permissions in the README.
- The README shows how to remove the aggregation label and bind the role narrowly.

Residual risk: access is all or nothing per namespace, and a cluster can hold only one install.

### Namespace-label writers grant visibility

A namespace sees the project when it has `harbor.goharbor.io/project=<project>` ([gate.go](../pkg/namespaces/gate.go)).
Anyone who can create namespaces or update their labels can grant that.
The built-in `admin` and `edit` roles cannot change a Namespace object, but many clusters let tenants set namespace labels in other ways:

- GitOps controllers with cluster-wide rights, such as Argo CD or Flux, create namespaces with the labels that a repository declares, and tenants may control that repository.
- Self-service tenancy tools, such as Capsule or Rancher projects, let tenants create namespaces, sometimes with labels they choose.
- Development clusters often let every user create namespaces.

Mitigations:

- Review who can `create`, `update`, and `patch` namespaces, and which tools create namespaces with labels that tenants choose.
- Restrict the label with the ValidatingAdmissionPolicy in the [README](../README.md#label-namespaces), which lets only one group set, change, or remove it.

Residual risk: visibility depends on cluster governance outside this server.
Removing the label revokes visibility as soon as each replica's namespace informer sees the change.

### Harbor cannot attribute reads to Kubernetes users

Harbor sees only the robot's polls, which no Kubernetes request triggers.
Harbor's audit log records writes, pushes, and pulls, but not API reads such as these ([basic.go](../../src/pkg/auditext/event/basic.go)).
If the cluster has an audit policy, the Kubernetes audit log records the user, verb, resource, and namespace of each request that kube-apiserver proxies.
Clusters that kubeadm or kind creates have no audit policy by default.
harbor-apiserver keeps no audit log of its own.

Residual risk: only the Kubernetes audit log can show which users read the project.
Direct calls to the Service leave no Kubernetes audit record.

### Direct calls to the Service

A pod can call the Service without going through kube-apiserver, as boundary 3 describes.
harbor-apiserver authenticates these calls, and authorizes them with a SubjectAccessReview, so they grant no extra access.
The e2e test `TestDelegatedAuthorization` checks this.
They do bypass kube-apiserver's audit log and its API Priority and Fairness.
harbor-apiserver caches authentication and authorization decisions for 10 seconds, so revoking a direct caller's access takes up to 10 seconds.
kube-apiserver authorizes the requests that it proxies itself, so revocation applies to them at once.

At `-v` 8 or higher, client-go logs request bodies, including the TokenReviews that carry direct callers' bearer tokens, so keep `-v` below 8.

Mitigations not in the manifests:

- A NetworkPolicy in `harbor-apiserver` that admits TCP port 6443 only from kube-apiserver and the kubelet.
  Selecting kube-apiserver is hard when it runs on the host network or in a managed control plane.
- Audit logging in harbor-apiserver, which would need the `k8s.io/apiserver` audit options that it does not expose.

### Robot credentials leak

The credentials could leak from the cluster, from the server's responses or logs, or in transit.

Mitigations:

- API responses describe Harbor failures only with fixed 503 messages ([store.go](../pkg/registry/store.go), [poll.go](../pkg/registry/poll.go), [registry.go](../pkg/registry/registry.go)): `harbor has not been read yet`, `harbor is unavailable`, `reading harbor takes longer than the staleness limit`, `reading a repository's artifacts failed`, `harbor rejected the robot account credentials`, `harbor denied the robot account access`, `not found in harbor`, or `unexpected error reading harbor`.
  They never include Harbor's URL, Harbor's response, or the credentials.
- The server logs the full error of a failed read of Harbor.
  That error holds the request URL, the HTTP status, and Harbor's error message ([client.go](../pkg/harbor/client.go)).
  The credentials travel only in the `Authorization` header, which no error includes, and `net/http` redacts any password in a URL.
  A credential file error names the file, not its contents.
  The Harbor client does not use client-go, so no log verbosity logs its requests.
- The client refuses redirects, so the credentials go only to `--harbor-url`.
- The container runs a distroless image as non-root, with a read-only root filesystem and no privilege escalation ([deployment.yaml](../deploy/base/deployment.yaml)).

Residual risk:

- Cluster admins, anyone who can read Secrets or create pods in `harbor-apiserver`, and admins of the nodes running the pods can read the credentials.
  etcd stores them unencrypted unless the cluster encrypts Secrets at rest.
- With an `http://` URL, which the README forbids but the server accepts, the secret crosses the network in clear text.
- If Harbor, or a proxy in front of it, echoed request headers in an error body, the log would include them.
- A leaked credential reads a little more than the server shows: Harbor's artifact list can also return labels, accessories, and vulnerability and SBOM overviews.

### A public project hides broken credentials

Harbor's security middleware tries each authenticator in turn ([security.go](../../src/server/middleware/security/security.go)).
A robot whose name or secret is wrong, or which is expired, disabled, or deleted, matches none ([robot.go](../../src/server/middleware/security/robot.go)), so Harbor handles the request as anonymous.
Anonymous users can list and read a public project.
The server keeps working, and nothing in Kubernetes shows the failure.

This exposes nothing new, because anyone who can reach Harbor can read a public project.
The failure surfaces only when the project becomes private, as 503 errors once the snapshot is older than `--harbor-staleness-limit`.

Mitigations: prefer a private project, track the robot's expiry, and watch harbor-core's logs for failed robot authentication.

### Changing the install namespace

`deploy/base/rbac.yaml` binds the Role `extension-apiserver-authentication-reader` in `kube-system`.
Kustomize's `namespace` field moves that RoleBinding into the new namespace, where the Role does not exist.
The server then cannot read the front-proxy CA, and exits at startup with `unable to load configmap based request-header-client-ca-file`.
It fails closed, but the APIService becomes unavailable, which breaks discovery cluster-wide.
The same field leaves the old namespace in the `cert-manager.io/inject-ca-from` annotation and in the serving Certificate's DNS names.
Kustomize applies the field after the patches in the same kustomization, so those patches cannot repair the RoleBinding.

Mitigations: keep the namespace `harbor-apiserver`; other namespaces are unsupported.
Never set `--authentication-tolerate-lookup-failure`, which lets the server start without front-proxy authentication when the lookup fails.

### APIService TLS

The aggregator sends every user's identity to the Service, and trusts what it returns.
It verifies the server against the APIService's `caBundle`; no manifest sets `insecureSkipTLSVerify`, and `hack/gen-serving-cert.sh` clears it.

- With [deploy/cert-manager](../deploy/cert-manager/certificates.yaml), cert-manager keeps a self-signed CA that lasts 10 years in Secret `harbor-apiserver-ca`, and renews the serving certificate.
  Its CA injector copies `ca.crt` from Secret `harbor-apiserver-tls` into the `caBundle`.
- With [hack/gen-serving-cert.sh](../hack/gen-serving-cert.sh), the CA lasts 10 years and the serving certificate lasts 1 year.
  Someone must rerun the script before the serving certificate expires, or the APIService becomes unavailable.

The server reloads a renewed serving certificate without a restart.

Residual risk: control of the `harbor-apiserver` namespace amounts to control of the aggregator's trust in this API.
Anyone who can read Secret `harbor-apiserver-ca` or `harbor-apiserver-tls`, write Secrets, or create cert-manager Certificates or CertificateRequests there can get a certificate that the aggregator trusts.
cert-manager approves requests to Issuer `harbor-apiserver-ca` by default.
With control of the Service's endpoints, such as by creating pods there, they could serve forged data and read user identities, but not user tokens.
The built-in `edit` and `admin` roles grant all of this, and cert-manager's Helm chart aggregates its Certificate and CertificateRequest rights into them.
Grant `edit` and `admin` in `harbor-apiserver` only to cluster admins.

### Front-proxy trust

harbor-apiserver reads the front-proxy CA, the allowed client names, and the header names from ConfigMap `kube-system/extension-apiserver-authentication`, and follows changes to it.
It trusts `X-Remote-User`, `X-Remote-Group`, and `X-Remote-Extra-*` only from a client certificate signed by that CA, and with an allowed name when the list is not empty.

Residual risk: any holder of a front-proxy client certificate with an allowed name can act as any user here, as on every aggregated API server.
Keep the front-proxy CA separate from the cluster CA, and set `--requestheader-allowed-names` on kube-apiserver.

### Denial of service against Harbor and the server

Kubernetes requests never call Harbor.
Instead, each replica polls Harbor ([poll.go](../pkg/registry/poll.go)).
Each poll makes one repository list, then one artifact list per repository, 4 at a time.
Each Harbor list pages through 100 items at a time, and starts over after 1 second, up to 3 tries in all, when the item count drops during it.
After a poll, the replica waits `--harbor-poll-interval` (30s) plus up to 10% jitter.
After a poll that fails, or that keeps a repository's artifacts from an earlier poll, it retries once after 1 second, and then waits the poll interval until a poll succeeds.

Limits:

- [client.go](../pkg/harbor/client.go) reads at most 1000 pages per list and 16 MiB per response, and fails the list beyond either.
- `--harbor-timeout` (10s) bounds each Harbor request, and `--harbor-staleness-limit` (5m) bounds each poll.
- Each replica has at most 4 Harbor requests in flight.
- Each replica serves at most 400 read requests at once.
- kube-apiserver's API Priority and Fairness applies to the requests it proxies, but not to direct calls to the Service.

Residual risk: Harbor's load grows with the project's size and the number of replicas, but not with Kubernetes requests.
The server ignores `limit` and `continue`.

The page and size limits do not bound memory in practice.
Each replica holds the project in memory ([store.go](../pkg/registry/store.go)).
A list holds every object in memory, once per labeled namespace, and then encodes the whole response.
The pods request 64 MiB of memory, and have no memory limit and no priority class.
So a list of a large project across all namespaces, such as from a monitoring or dashboard ServiceAccount with cluster-wide `list`, or a few concurrent lists, can grow a pod far beyond its request.
Under node memory pressure, such a pod is among the first that the kubelet evicts or the kernel kills, and other pods on the node suffer too.
If both replicas go, the APIService becomes unavailable, which breaks discovery and namespace deletion cluster-wide.
Not yet done: a memory limit with headroom, a cap on the objects or bytes in a response that fails the request instead of exhausting memory, and `priorityClassName: system-cluster-critical`, as metrics-server uses.

### Harbor outages

A failed poll leaves the replica's last good snapshot in place ([poll.go](../pkg/registry/poll.go)).
The replica serves that snapshot until it is older than `--harbor-staleness-limit` (5m).
Then it fails requests for labeled namespaces with 503 ([store.go](../pkg/registry/store.go)).
Connection errors, timeouts before Harbor responds, and 429 and 5xx responses give the message `harbor is unavailable`.
A timeout or dropped connection while the server reads the repository list's response gives `unexpected error reading harbor` instead ([client.go](../pkg/harbor/client.go)).
A poll that runs longer than `--harbor-staleness-limit` fails, and gives `reading harbor takes longer than the staleness limit`.

Some failures of one repository's artifact list, such as a timeout while the server reads Harbor's response, or more than 1000 pages, don't fail the poll.
The replica then keeps that repository's artifacts from an earlier poll, or none if no earlier poll listed the repository.
Once they are older than `--harbor-staleness-limit`, or at once on a pod's first poll, requests that could return them fail with `reading a repository's artifacts failed`.
Other requests still succeed: those for `HarborRepository` objects, and those for artifacts that a get's name, a `harbor.goharbor.io/repository` label selector, or an equality `status.repository` or `metadata.name` field selector confines to other repositories ([artifacts.go](../pkg/registry/artifacts.go)).

A pod becomes ready once it has listed namespaces and its first poll has ended, in success or failure ([server.go](../cmd/harbor-apiserver/server.go)).
An outage ends that poll within about `--harbor-timeout`.
So the APIService stays Available, discovery keeps working, and pods can restart during an outage.
A pod that starts during an outage fails requests with 503 until it reads Harbor.
The e2e test `TestHarborOutageIsServiceUnavailable` checks this behavior.

Residual risk: during an outage, clients get data up to `--harbor-staleness-limit` old, without an error.
If the image is hosted in the same Harbor, new pods cannot pull it during the outage.
A slow Harbor or a large project can keep a new pod unready for up to `--harbor-staleness-limit`.
If all replicas restart then, the APIService is unavailable for that long.

### Untrusted Harbor data

Project members who can push or update repositories control repository names, descriptions, tags, and OCI annotations.
The server copies them into `status` without interpreting them.
It derives object names that fit Kubernetes rules, and skips an artifact whose digest cannot form a name.
Clients that display these fields should treat them as untrusted.

Residual risk: a pusher can make one repository's artifact list fail on every poll, such as by pushing more than 100000 artifacts, or artifacts whose annotations make a page exceed 16 MiB.
Once `--harbor-staleness-limit` passes, every replica fails requests that could return that repository's artifacts, such as unfiltered lists of artifacts, as in [Harbor outages](#harbor-outages).

Object names hold only part of what they name: an artifact's name holds 12 hex digits of its digest, and a hash suffix holds 10 hex digits of the repository name's hash ([names.go](../pkg/registry/names.go)).
A pusher can grind a manifest whose digest shares its first 12 hex digits with one that will be pushed later, such as a mirrored upstream image, in about 2^48 SHA-256 operations.
Pushed first, it takes the object name, and the server hides the later artifact.
Clients should check `status.digest`, or `status.name` for repositories, rather than trust an object name.
