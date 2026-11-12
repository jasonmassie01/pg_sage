# pg-sage Helm chart

Runs the pg_sage sidecar on Kubernetes: one Deployment (one replica,
`Recreate`), its ConfigMap, a Service, and optionally a ServiceMonitor, a
NetworkPolicy and the read-only Role of the CloudNativePG adapter.

```sh
kubectl create secret generic pg-sage-secrets \
  --from-file=database-url=./database-url \
  --from-file=llm-api-key=./llm-api-key \
  --from-file=encryption-key=./encryption-key
helm install sage deploy/helm/pg-sage \
  --set secrets.existingSecret=pg-sage-secrets \
  --set secrets.llmAPIKeyKey=llm-api-key \
  --set secrets.encryptionKeyKey=encryption-key
```

The chart never creates a Secret. The database URL, the LLM key and the
encryption key are mounted as files and passed as `SAGE_*_FILE`, so they stay
out of the process environment and rotate in place.

## What the chart guarantees

- **One sidecar.** `replicas: 1` and `strategy: Recreate` are fixed, not
  values: two sidecars on the same databases would run executors together.
- **A pinned image.** The tag defaults to the chart `appVersion`, which the
  release job sets to the release. `image.tag: latest` fails the render.
- **Least privilege.** The pod runs as uid/gid 1000 with a read-only root
  filesystem, no privilege escalation, all capabilities dropped and the
  `RuntimeDefault` seccomp profile. The service-account token is mounted only
  when the CloudNativePG adapter is on.
- **Hot reload.** The ConfigMap is mounted as a directory (no `subPath`), so an
  edit reloads without a restart ([ConfigMap edits and hot reload](../../../docs/deployment.md#configmap-edits-and-hot-reload)).

## CloudNativePG

On a CloudNativePG Cluster the operator owns `postgresql.conf` and disables
`ALTER SYSTEM`. Name the Cluster and pg_sage turns instance settings into
Cluster spec proposals: a `kubectl patch cluster ... --type merge` command, its
rollback and a GitOps fragment, which an operator approves and applies.
pg_sage never patches the Cluster.

```sh
helm install sage deploy/helm/pg-sage \
  --set secrets.existingSecret=pg-sage-secrets \
  --set cnpg.cluster=pg-main
```

`cnpg.cluster` sets `postgres.cnpg` in the rendered config (standalone mode).
The chart then creates a Role in the Cluster's namespace that grants only `get`
on `clusters.postgresql.cnpg.io`, limited to the named Clusters, and binds it
to the chart's service account. In fleet mode, set `databases[].cnpg` in
`config` and list each Cluster in `cnpg.rbac.clusters`.

A change that needs a restart (`shared_buffers`, `max_connections`) makes the
operator restart the replicas one at a time and then the primary. Under
`primaryUpdateStrategy: unsupervised` (the default) that is a switchover or an
in-place restart of the primary, a short write outage. See
[CloudNativePG](../../../docs/managed-clouds.md#cloudnativepg).

## Values

| Value | Default | Meaning |
|---|---|---|
| `image.repository` | `ghcr.io/jasonmassie01/pg_sage` | Image repository |
| `image.tag` | `""` (the appVersion) | Image tag; `latest` is refused |
| `image.pullPolicy` | `IfNotPresent` | Pull policy |
| `imagePullSecrets` | `[]` | Pull secrets |
| `nameOverride`, `fullnameOverride` | `""` | Object names |
| `serviceAccount.create`, `.name`, `.annotations` | `true`, `""`, `{}` | The service account |
| `config` | standalone, API on 8080, metrics on 8187 | pg_sage's `config.yaml` ([configuration](../../../docs/configuration.md)) |
| `secrets.existingSecret` | `<fullname>-secrets` | Secret holding the files below |
| `secrets.databaseURLKey` | `database-url` | Key of the database URL (`SAGE_DATABASE_URL_FILE`) |
| `secrets.llmAPIKeyKey` | `""` | Key of the LLM API key (`SAGE_LLM_API_KEY_FILE`) |
| `secrets.encryptionKeyKey` | `""` | Key of the encryption passphrase (`SAGE_ENCRYPTION_KEY_FILE`) |
| `persistence.existingClaim` | `""` (an emptyDir) | Claim for `data_dir` (`/var/lib/pg_sage`) |
| `tls.existingSecret` | `""` | `kubernetes.io/tls` Secret; the API and probes switch to HTTPS |
| `cnpg.cluster` | `""` | CloudNativePG Cluster of the standalone database |
| `cnpg.namespace` | `""` (the release namespace) | Namespace of the Clusters |
| `cnpg.rbac.create` | `true` | Create the get-only Role and its binding |
| `cnpg.rbac.clusters` | `[]` | More Clusters the Role names (fleet mode) |
| `service.type`, `service.annotations` | `ClusterIP`, `{}` | The Service (ports 8080 `api`, 8187 `metrics`) |
| `resources` | 100m / 256Mi, limit 1Gi | Container resources |
| `podAnnotations`, `podLabels` | `{}` | Pod metadata |
| `nodeSelector`, `tolerations`, `affinity`, `priorityClassName` | empty | Scheduling |
| `serviceMonitor.enabled`, `.interval`, `.labels` | `false`, `30s`, `{}` | Prometheus Operator ServiceMonitor |
| `networkPolicy.enabled`, `.from` | `false`, `[]` | Ingress policy for the API and metrics ports; empty `from` allows any peer |

Without `secrets.encryptionKeyKey`, pg_sage generates
`/var/lib/pg_sage/encryption.key` and seals stored secrets with it. On the
default emptyDir a pod restart loses that key and the secrets sealed under it:
set an encryption key or `persistence.existingClaim`.

## Tests

`sidecar/internal/config/helm_chart_test.go` renders the chart with Go's
`text/template` and the Helm functions it uses, and loads the rendered
`config.yaml` with pg_sage's own loader. CI also runs `helm lint`,
`helm template` and kubeconform on the files in `tests/`.
