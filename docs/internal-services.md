# Fieldguide internal-services deployment

The Fieldguide deployment runs Akari as a private Kubernetes service in the
`akari` namespace. It has no ingress or DNS record. Forge reaches it at
`http://fg-akari.akari.svc.cluster.local` and is responsible for the
external origin, TLS, and user authentication.

Infrastructure stores the runtime values under `/akari/internal-services` in
AWS Systems Manager Parameter Store. The deployment workflow projects those
values into the `fg-akari-runtime` Kubernetes Secret immediately before each
Helm upgrade. Akari receives:

- `AKARI_DATABASE_URL` for its standalone PostgreSQL 18 instance;
- `AKARI_LISTEN=:8080`;
- `AKARI_PROXY_AUTH_HEADER=X-Forge-User`;
- `AKARI_PROXY_AUTH_SECRET_HEADER=X-Akari-Proxy-Secret`;
- `AKARI_PROXY_AUTH_SECRET`, shared with Forge through its own SSM prefix.

The Helm chart creates a ClusterIP service and an ingress NetworkPolicy that
selects the Forge pods in `applications`. Akari's GitHub deployment role is
bound to an RBAC Role in the `akari` namespace; it has no administrative access
to Forge or other cluster workloads. The shared proxy secret remains required
because network policy enforcement depends on the cluster CNI and another
in-cluster workload must not be able to forge an identity header.

## Account bootstrap and proxy behavior

The deployment deliberately leaves the database without an account. Akari's
first local registration remains the supported way to create its admin account.
Forge must provide a separate forwarding path for local login and registration
that does not set `X-Forge-User`; ordinary application traffic should overwrite
that header with the authenticated Forge identity and include the shared secret.

The no-identity path must preserve the browser `Host`, `Origin`, `Sec-Fetch-Site`,
and `X-Forwarded-Proto` semantics Akari validates. Keep the Akari ClusterIP
unreachable outside the cluster, and never forward a client-supplied
`X-Forge-User` or `X-Akari-Proxy-Secret` value.

## Releases

A version tag publishes the normal GitHub Release, then calls
`deploy-internal-services.yml`. The deploy job assumes the release deployment
`akari-release-deploy` AWS role, builds the tagged Dockerfile into
`fg-akari-internal-services`, refreshes the Kubernetes Secret from SSM, and runs
an atomic Helm upgrade. Akari applies embedded database migrations before it
starts serving.

For the initial deployment or a deliberate rollback, run the `Deploy internal
services` workflow manually with a published version tag. Leaving the input
empty deploys the latest published release.
