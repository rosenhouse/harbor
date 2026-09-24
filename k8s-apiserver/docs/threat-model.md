# Threat model

harbor-apiserver shows one Harbor project's metadata to Kubernetes users.
With the component [deploy/components/replication](../deploy/components/replication/kustomization.yaml), it also lets them copy images into the project.
This model covers an install from `deploy/`, with or without that component, as the [README](../README.md) describes.

## Assets

- Secret `harbor-apiserver` holds the project robot's credentials.
  With the README's permissions, they can list repository and artifact metadata in one project, but cannot pull, push, or change anything.
- Secret `harbor-apiserver-replication`, from the replication component, holds a system-level robot account's credentials.
  With the README's permissions, they can list registry endpoints, and create, start, stop, and delete replication policies anywhere in Harbor.
- The project's content and storage quota are at stake, because replications write into the project.
- The project metadata includes repository names and descriptions, artifact digests, tags, sizes, media types, OCI annotations, and push and pull times and counts.
- Secret `harbor-apiserver-ca` holds the serving CA's key, and Secret `harbor-apiserver-tls` holds the serving key.
  The aggregator trusts every serving certificate that the CA signs for this API.
- The cluster's API depends on this server, because an unavailable APIService breaks discovery and namespace deletion cluster-wide.
- Harbor's availability is at stake, because each replica reads the whole project from Harbor every poll interval.
  Replication requests call Harbor directly, and each replication runs jobs in Harbor.

## Actors

- **Namespace users** can get or list the kinds in a namespace, usually through the `view`, `edit`, or `admin` role.
- **Namespace-label writers** can create namespaces or change their labels, directly or through a tool.
- **Cluster admins** can read Secrets and change RBAC, the APIService, and the `harbor-apiserver` namespace.
- **Replication creators** can create and delete replications in a labeled namespace, usually through `harbor.goharbor.io:replicate`.
- **Harbor admins** manage the project, its visibility, the robot accounts, and the registry endpoints.
- **Project pushers** control repository names, digests, tags, and annotations.
- **Source publishers** control the images in the source repositories that replications copy.
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
   Every read of the project carries the project robot's credentials as HTTP basic auth.
5. **harbor-apiserver to kube-apiserver.**
   The ServiceAccount can list and watch namespaces, create TokenReviews and SubjectAccessReviews, and read ConfigMap `kube-system/extension-apiserver-authentication` ([rbac.yaml](../deploy/base/rbac.yaml)).
6. **harbor-apiserver to Harbor, as the replication robot.**
   Replication requests, and each poll's list of replication policies, carry the replication robot's credentials to the same URL as in boundary 4.
   Harbor authorizes them by system permission alone, and checks neither the project nor the registry endpoints of a policy ([replication.go](../../src/server/v2.0/handler/replication.go)).
   The server enforces those itself.
   For each run, harbor-core lists the source repository's tags at the endpoint, and matches them against the tag pattern.
   Harbor's job service then pulls from the endpoint with the endpoint's credentials, and writes into the project as Harbor.

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
With replications, the label also lets the namespace's replication creators write into the project.
The built-in `admin` and `edit` roles cannot change a Namespace object, but many clusters let tenants set namespace labels in other ways:

- GitOps controllers with cluster-wide rights, such as Argo CD or Flux, create namespaces with the labels that a repository declares, and tenants may control that repository.
- Self-service tenancy tools, such as Capsule or Rancher projects, let tenants create namespaces, sometimes with labels they choose.
- Development clusters often let every user create namespaces.

Mitigations:

- Review who can `create`, `update`, and `patch` namespaces, and which tools create namespaces with labels that tenants choose.
- Restrict the label with the ValidatingAdmissionPolicy in the [README](../README.md#label-namespaces), which lets only one group set, change, or remove it.

Residual risk: visibility depends on cluster governance outside this server.
Removing the label revokes visibility as soon as each replica's namespace informer sees the change.
It also hides the namespace's replications, but leaves them running (see [Removing the label leaves replications running](#removing-the-label-leaves-replications-running)).

### Harbor cannot attribute reads to Kubernetes users

For reads of the project, Harbor sees only the project robot's polls, which no Kubernetes request triggers.
Harbor's audit log records writes, pushes, and pulls, but not API reads such as these ([basic.go](../../src/pkg/auditext/event/basic.go)).
If the cluster has an audit policy, the Kubernetes audit log records the user, verb, resource, and namespace of each request that kube-apiserver proxies.
Clusters that kubeadm or kind creates have no audit policy by default.
harbor-apiserver keeps no audit log of its own.

Harbor cannot attribute replications either.
It records the replication robot as the creator of every policy that the server creates, and its audit log does not record replication policy changes.

Residual risk: only the Kubernetes audit log can show which users read the project, or created and deleted replications.
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
- Replication requests use fixed messages too ([replications.go](../pkg/registry/replications.go)): `harbor is unavailable` (503), `harbor rejected the robot account credentials`, `harbor denied the robot account access`, or `unexpected error from harbor` (500), and `harbor rejected the replication policy` (400).
  Some create errors name the Harbor policy, whose name comes from the replication's namespace and name.
  Lists of artifacts by the replication label can fail with `listing replication policies failed`, followed by one of the 503 messages above.
- A replication's `status.lastExecution.message` is Harbor's text for its last run, unfiltered.
  It can hold endpoint URLs, such as Harbor's own in-cluster URL, and the source registry's responses, and anyone who can view the replication can read it.
- The server logs the full error of a failed Harbor request.
  That error holds the request URL, the HTTP status, and Harbor's error message ([client.go](../pkg/harbor/client.go)).
  The credentials travel only in the `Authorization` header, which no error includes, and `net/http` redacts any password in a URL.
  A credential file error names the file, not its contents.
  The Harbor client does not use client-go, so no log verbosity logs its requests.
- The client refuses redirects, so both robots' credentials go only to `--harbor-url`.
- The container runs a distroless image as non-root, with a read-only root filesystem and no privilege escalation ([deployment.yaml](../deploy/base/deployment.yaml)).

Residual risk:

- Cluster admins, anyone who can read Secrets or create pods in `harbor-apiserver`, and admins of the nodes running the pods can read the credentials.
  With replications, that includes the replication robot's credentials, which control replication across Harbor (see [The replication robot controls replication across Harbor](#the-replication-robot-controls-replication-across-harbor)).
  etcd stores them unencrypted unless the cluster encrypts Secrets at rest.
- With an `http://` URL, which the README forbids but the server accepts, the secret crosses the network in clear text.
- If Harbor, or a proxy in front of it, echoed request headers in an error body, the log would include them.
- A leaked project robot credential reads a little more than the server shows: Harbor's artifact list can also return labels, accessories, and vulnerability and SBOM overviews.

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

### The replication robot controls replication across Harbor

Harbor checks the replication robot's permissions only at the system level ([replication.go](../../src/server/v2.0/handler/replication.go)).
It cannot limit the robot to one project, to one registry endpoint, or to pulling.
With the robot's credentials, anyone can:

- Push any project to any registry endpoint, with the endpoint's credentials.
- Pull from any endpoint into any project, replacing its tags, or into a new project, which Harbor creates ([adapter.go](../../src/pkg/reg/adapter/harbor/base/adapter.go)).
  So they can replace any tag in Harbor with any image that an endpoint reaches, such as a public image on Docker Hub.
- Copy anything that an endpoint's credentials can read, such as a private upstream repository, into any project, including a public one.
  Through an endpoint that points back at this Harbor, that includes every private project that the endpoint's credentials can read.
- Start, stop, or delete any replication policy, including those that Harbor admins created.
- List the registry endpoints, with their URLs and access keys, but not their secrets.
- Read every policy's description, which holds a replication's metadata (see [Harbor admins see replication metadata](#harbor-admins-see-replication-metadata)).

The server uses the robot more narrowly ([replication_policy.go](../pkg/registry/replication_policy.go), [replications.go](../pkg/registry/replications.go)):

- It creates only pull policies from an allowed endpoint into `<project>/<prefix>/<namespace>/<name>`, which don't replicate deletions.
- It never updates a policy.
- It shows, stops, or deletes a policy, or links artifacts to it, only if all of these hold:
  - The policy's name is `<prefix>.<project>.<namespace>.<name>`, and its description names that namespace and name, and says that harbor-apiserver manages it.
  - Its destination is `<project>/<prefix>/<namespace>/<name>` in the local Harbor, and keeps each source repository's full path.
  - Its source is the allowed endpoint that the description's spec names.
  - Its filters and trigger are those that the server creates from that spec.
  - The namespace sees the project, and has the UID that the description records.
- It stops only the running executions of a policy that it is deleting.
  The Harbor client drops any execution of another policy that Harbor returns ([replication.go](../pkg/harbor/replication.go)).

Mitigations:

- Enable replications only where you need them. The base install holds no such credentials.
- Give the robot only the README's permissions, and an expiration.
- Give every registry endpoint in Harbor credentials that can only read content that anyone who can reach Harbor may see, or none.
  Then no policy can push through an endpoint, or expose private content by copying it.
  Never give an endpoint that points back at this Harbor the credentials of an account that can read private projects.
- Grant `edit` and `admin` in `harbor-apiserver` only to cluster admins, as for [APIService TLS](#apiservice-tls).

Residual risk: whoever reads Secret `harbor-apiserver-replication` can do all of the above.

### Replications bring external content into a shared project

Replications write into the project that every labeled namespace sees.
A replication creator in one namespace chooses content that every labeled namespace lists as `HarborArtifact` objects, and that anyone who can pull from the project can pull.
Source publishers control that content: each run copies whatever the matching tags point to at the time, and replaces the tags that an earlier run copied.
A replication creator can copy anything that the endpoint's credentials can read, and so expose private content, such as a private upstream repository.
The path `<prefix>/<namespace>/<name>/` keeps replications out of repositories that people push to, and out of each other's.
But project pushers can push under that path too, and their artifacts then get the replication's label and ownerReference ([store.go](../pkg/registry/store.go)).

Replicated images count against the project's storage quota, which every labeled namespace and every pusher shares.
One replication with `tag: "*"` of a large repository can fill it, and then pushes to the project fail.
Without a quota, it can fill Harbor's storage.

Mitigations:

- Bind `harbor.goharbor.io:replicate` only to users whom you trust to choose content for every labeled namespace.
- Allow only endpoints whose content you trust, and whose credentials can only read what everyone who can pull from the project may see.
- Set a storage quota on the project.
- Clients should pull by digest or verify signatures, rather than trust a tag or the replication label.

Residual risk: the server does not limit how many replications a namespace has, or how much they copy (see [Denial of service](#denial-of-service-against-harbor-and-the-server)).

### Removing the label leaves replications running

Removing `harbor.goharbor.io/project` from a namespace, or changing it, hides the namespace's replications ([replication_policy.go](../pkg/registry/replication_policy.go)).
Their policies stay in Harbor, and their schedules keep running.
Kubernetes users can no longer see or delete them, and deleting the namespace no longer deletes them.
Restoring the label shows them again.
The same happens to the replications of an endpoint that `registries` no longer allows.
It happens to all replications when `--replication-prefix` changes or replications are disabled.
It also happens to a policy when someone changes, in Harbor, what the server checks before it shows the policy (see [The replication robot controls replication across Harbor](#the-replication-robot-controls-replication-across-harbor)).
Other changes in Harbor leave the policy visible.
The server shows a change to the description's UID, labels, or annotations as a change to the replication, even to its UID.
It doesn't show other changes, such as disabling the policy.

Mitigations: delete the replications before you remove a label, an endpoint, or the component.
A Harbor administrator can find the policies by their name prefix, `<prefix>.<project>.<namespace>.`, and delete them.

### Harbor admins see replication metadata

Each policy's description holds its replication's namespace, namespace UID, name, UID, labels, annotations, and spec, as JSON ([replication_policy.go](../pkg/registry/replication_policy.go)).
After `kubectl apply`, the annotations include `kubectl.kubernetes.io/last-applied-configuration`, which repeats the object.
Harbor system admins, system robots that can read replication policies, and anyone with the replication robot's credentials can read the descriptions, in Harbor's UI under **Administration** > **Replications** or through its API.

Mitigation: don't put secrets in a replication's labels or annotations.

### A recreated namespace does not inherit replications

A namespace that is deleted and created again gets a new UID.
Each policy's description records the UID of the namespace that created it, and the server hides a policy whose namespace now has another UID.
Artifact links check the UID too ([store.go](../pkg/registry/store.go)).
So a new namespace with the old name cannot see or delete the old namespace's replications, and doesn't see their labels on artifacts.

The namespace controller normally deletes a namespace's replications with the namespace (see [Namespace deletion waits for Harbor](#namespace-deletion-waits-for-harbor)).
A policy outlives its namespace only if the server hid it first (see [Removing the label leaves replications running](#removing-the-label-leaves-replications-running)), the server was uninstalled first, or a create raced with the namespace's deletion.
Such a policy keeps running, and blocks its namespace and name.
A create there fails with AlreadyExists, and the message names the Harbor policy and says that a Harbor administrator must delete it ([replications.go](../pkg/registry/replications.go)).

### Namespace deletion waits for Harbor

Before it deletes a namespace, the namespace controller deletes every object in it, including replications, through this server.
The server deletes each replication as it deletes any other ([replications.go](../pkg/registry/replications.go)).
Harbor refuses to delete a policy while one of its executions runs, so the server stops the running executions, and retries every second.
After 30 seconds, or when the request ends, it returns Conflict, and the namespace controller retries later.
The namespace stays Terminating until every delete succeeds.

Listing a labeled namespace's replications calls Harbor, even when there are none.
It lists only that namespace's policies, so other namespaces' replications cannot delay its deletion.
So while Harbor is unavailable, or rejects the replication robot, such as after it expires, no labeled namespace can finish deleting.

Mitigations: track the replication robot's expiry.
If Harbor cannot recover, removing a Terminating namespace's label lets it finish deleting, and leaves its policies in Harbor for a Harbor administrator to delete.

### Replications skip admission

The server runs no admission plugins, and kube-apiserver does not run admission for requests that it proxies to an aggregated API server.
So admission webhooks, such as those of Kyverno or Gatekeeper, ValidatingAdmissionPolicies, and ResourceQuota never see replications.
The server's validation, the endpoint allow-list, the namespace label, and RBAC are the only checks on a replication.
The server refuses creates in a terminating namespace itself, as kube-apiserver's NamespaceLifecycle admission does.

Residual risk: every replication creator can use every allowed endpoint, and any repository and tag there.

### Two clusters that share a project

Each server shows only the policies whose namespace UID matches one of its cluster's namespaces, so clusters don't see each other's replications.
Clusters that share a project must set different `--replication-prefix` values.
With the same prefix, a replication in one cluster blocks the same namespace and name in the other, and the error there asks a Harbor administrator to delete the first cluster's policy.
Users of each cluster see every artifact in the project, including those that the other cluster's replications copied, but without the replication label.
Each cluster's replication robot can stop or delete the other cluster's policies through Harbor's API, although neither server does.
The clusters share the project's quota.

### Denial of service against Harbor and the server

Requests for repositories and artifacts never call Harbor.
Instead, each replica polls Harbor ([poll.go](../pkg/registry/poll.go)).
Each poll makes one repository list, then one artifact list per repository, 4 at a time.
With replications, each poll then lists the replication policies.
Each Harbor list pages through 100 items at a time, and starts over after 1 second, up to 3 tries in all, when the item count drops during it.
After a poll, the replica waits `--harbor-poll-interval` (30s) plus up to 10% jitter.
After a poll that fails, or that keeps a repository's artifacts from an earlier poll, it retries once after 1 second, and then waits the poll interval until a poll succeeds.

Requests for replications call Harbor directly, as the replication robot ([replications.go](../pkg/registry/replications.go)):

- A `get` makes one policy list and one execution read.
  An execution read lists 10 executions at a time, until it finds one that Harbor did not skip.
- A `list` makes one list of the policies of its namespace, or of every namespace, then one execution read per replication that it returns, 4 at a time.
- A `create` lists the registry endpoints, then creates, reads, and starts the policy, and reads its execution.
  If a step after the create fails, it deletes the policy, even after the request has ended, first listing the policies if Harbor did not return the policy's ID.
  A dry run lists the policies instead of creating one.
- An `update` or `patch` finds the replication as a `get` does, and changes nothing. A server-side apply of a missing replication creates it as a `create` does.
- A `delete` finds the replication as a `get` does, then deletes the policy.
  While Harbor refuses, because an execution runs, it lists and stops the running executions, and retries every second for up to 30 seconds.
- Each replication runs once when it is created, and then on its schedule.
  Each run lists the source repository's tags in harbor-core, and matches each tag against the tag pattern.
  harbor-core runs at most 10 of these flows at once per replica, for all of Harbor, and deleting a replication does not stop a flow that is running ([execution.go](../../src/controller/replication/execution.go)).
  Then the run copies the matching artifacts in Harbor's job service, which all of Harbor shares.

Limits:

- [client.go](../pkg/harbor/client.go) reads at most 1000 pages per list and 16 MiB per response, and fails the list beyond either.
- A replication's labels and annotations total at most 8 KiB, so a page of 100 policies stays below 16 MiB, even when escaping grows each byte to 7.
- `--harbor-timeout` (10s) bounds each Harbor request, and `--harbor-staleness-limit` (5m) bounds each poll.
- Each poll has at most 4 Harbor requests in flight.
- Each replication `list` has at most 4 Harbor requests in flight.
- Each replica serves at most 400 read requests and 200 other requests at once.
- A schedule runs a replication at most once an hour. On Harbor 2.14 or later, runs of one replication don't overlap.
- A schedule must match a date that exists. Harbor's job service loops forever on one that never runs, such as February 30 ([enqueuer.go](../../src/jobservice/period/enqueuer.go)).
- A tag pattern has at most two `*`, and no `{}` group. Harbor's matcher tries every way to match, so each `*` multiplies its time by up to the tag's length, and each `{}` group by its number of alternatives.
  With these limits, it matches a 128-character tag in about 5 ms at worst ([replications_test.go](../pkg/registry/replications_test.go)).
- kube-apiserver's API Priority and Fairness applies to the requests it proxies, but not to direct calls to the Service.

Residual risk: the polls' load on Harbor grows with the project's size and the number of replicas, but not with Kubernetes requests.
Replication requests load Harbor in proportion to their rate.
The server does not limit the number of replications.
Many replications slow cluster-wide lists of replications, and each poll, which waits for its policy list.
Beyond 100,000, those policy lists fail.
Then cluster-wide lists of replications fail, and, once `--harbor-staleness-limit` passes, so do lists of artifacts by the `harbor.goharbor.io/replication` label.
Lists of replications in one namespace read only its policies, so other namespaces' replications don't affect them.
A replication creator can load Harbor's job service, which also runs garbage collection, scans, and other replications, with many replications, or with one of a large repository and `tag: "*"`.
A source repository with many long tags still costs harbor-core up to about 5 ms per tag in each run, and many such replications can occupy its 10 flows, which delays every replication in Harbor.
The server ignores `limit` and `continue`.

The page and size limits do not bound memory in practice.
Each replica holds the project in memory ([store.go](../pkg/registry/store.go)).
A list holds every object in memory, once per labeled namespace, and then encodes the whole response.
Each poll also holds every replication policy of the project, each with up to 8 KiB of labels and annotations, and so does a cluster-wide list of replications.
Nothing limits the number of replications, so replication creators can grow every replica's memory, by about 80 MiB for 10,000 replications with the most metadata.
The pods request 64 MiB of memory, and have no memory limit and no priority class.
So a list of a large project across all namespaces, such as from a monitoring or dashboard ServiceAccount with cluster-wide `list`, or a few concurrent lists, can grow a pod far beyond its request.
Under node memory pressure, such a pod is among the first that the kubelet evicts or the kernel kills, and other pods on the node suffer too.
If both replicas go, the APIService becomes unavailable, which breaks discovery and namespace deletion cluster-wide.
The install doesn't yet set a memory limit with headroom, or `priorityClassName: system-cluster-critical` as metrics-server does.
The server doesn't yet cap the number of replications, or the objects or bytes in a response, which would fail the request instead of exhausting memory.

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
Other requests still succeed: those for `HarborRepository` objects, and those for artifacts that a get's name, a `harbor.goharbor.io/repository` or `harbor.goharbor.io/replication` label selector, or an equality `status.repository` or `metadata.name` field selector confines to other repositories ([artifacts.go](../pkg/registry/artifacts.go)).

A failed list of the replication policies doesn't fail the poll.
Artifacts keep their links to replications from the last list that succeeded ([store.go](../pkg/registry/store.go)).
Once that list is older than `--harbor-staleness-limit`, lists of artifacts by the `harbor.goharbor.io/replication` label fail with `listing replication policies failed`, and other requests still succeed, with the old links.
Requests for replications read Harbor directly, so they fail during an outage with `harbor is unavailable` or `unexpected error from harbor`.
Labeled namespaces cannot finish deleting until Harbor recovers (see [Namespace deletion waits for Harbor](#namespace-deletion-waits-for-harbor)).

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
The source registry controls part of each replication's `status.lastExecution.message`, which can quote its responses.
Clients that display these fields should treat them as untrusted.

Residual risk: a pusher can make one repository's artifact list fail on every poll, such as by pushing more than 100000 artifacts, or artifacts whose annotations make a page exceed 16 MiB.
Once `--harbor-staleness-limit` passes, every replica fails requests that could return that repository's artifacts, such as unfiltered lists of artifacts, as in [Harbor outages](#harbor-outages).

Object names hold only part of what they name: an artifact's name holds 12 hex digits of its digest, and a hash suffix holds 10 hex digits of the repository name's hash ([names.go](../pkg/registry/names.go)).
A pusher can grind a manifest whose digest shares its first 12 hex digits with one that will be pushed later, such as a mirrored upstream image, in about 2^48 SHA-256 operations.
Pushed first, it takes the object name, and the server hides the later artifact.
Clients should check `status.digest`, or `status.name` for repositories, rather than trust an object name.
