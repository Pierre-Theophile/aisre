# The Kubernetes feeder, in the cluster it watches

`aisre feed k8s` runs as a pod inside the cluster and pushes what it sees to a remote `serve`
over https. Only outbound calls: no port opens in the cluster, the API server stays private,
and no secret is stored. The feeder authenticates with a projected ServiceAccount token
(audience `aisre`), which the kubelet rotates and the feeder re-reads on every call
(`--token-file`). `serve` trusts the cluster's issuer and maps the feeder's exact subject onto
the `feeder` role and its source (`--trusted-issuers`).

The threat model and the checks that answer it are in the header of
[`internal/server/auth_workload.go`](../../internal/server/auth_workload.go), each with a
test.

## 1. In the cluster

```sh
kubectl apply -f deploy/kind/rbac/namespace.yaml -f deploy/kind/rbac/serviceaccount.yaml \
  -f deploy/kind/rbac/clusterrole.yaml -f deploy/kind/rbac/clusterrolebinding.yaml
# edit AISRE_SERVER and AISRE_CLUSTER_NAME in deployment.yaml, then:
kubectl apply -f deploy/k8s-feeder/deployment.yaml
```

The role is read-only and reads no Secret (`--secrets off`). Whoever may create pods,
deployments or `serviceaccounts/token` in namespace `sre-agent` can act as the feeder, so keep
that namespace's RBAC to the cluster's owner.

## 2. In `serve`

Copy the cluster's public keys and its issuer. Neither is secret, but the key file decides
who is trusted, so it belongs with the deployment's configuration, where no feeder can write:

```sh
kubectl get --raw /.well-known/openid-configuration   # "issuer": the value for `issuer:`
kubectl get --raw /openid/v1/jwks > verda-jwks.json
```

Then write the trust file, from
[`trusted-issuers.example.yaml`](./trusted-issuers.example.yaml), and start `serve` with it:

```sh
aisre serve --migrate --auth oidc --trusted-issuers trusted-issuers.yaml
```

`--trusted-issuers` works alone, or alongside `--oidc-issuer` for people: a token whose
issuer the file lists is judged by the file only, and any other goes to the OIDC provider.
`serve` logs each trusted issuer at start-up with the thumbprints of its keys; compare them
with the cluster's when in doubt.

## Rules the trust file enforces

- **Every entry needs an `audience`.** The pod's ordinary Kubernetes token is refused, and so
  is any token minted for another service.
- **The key decides, not the issuer URL.** Every k3s and kubeadm cluster defaults to
  `https://kubernetes.default.svc.cluster.local`. A token from another cluster verifies only
  against its own entry's keys, and one key may not be trusted by two entries of the same
  issuer. A unique `--service-account-issuer` per cluster is better still.
- **Subjects match exactly.** There are no wildcards, and roles and the source come from the
  file, never from the token.
- **`max_lifetime` caps `exp − iat`** (default 1h), so a token minted with
  `kubectl create token --duration 8760h` is refused. A token without `exp` or `iat` is
  refused too.
- **Only asymmetric keys and algorithms.** The default is RS256 and ES256, and a `jwks_url`
  must be https.

## Operating it

- **Revoke** by removing the subject or the entry and restarting `serve`. Verification is
  offline, so a deleted ServiceAccount is not seen; the short lifetime bounds what that costs.
- **When the cluster rotates its signing key**, authentication fails closed and the feeder
  logs the refusal. Add the new key to the JWKS file, restart, then remove the old key.
- **The feeder refuses to send its token over plaintext http** to anything but loopback.
  `--insecure-transport` overrides that, for a lab network you trust.
