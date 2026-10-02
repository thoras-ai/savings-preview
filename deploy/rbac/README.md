# Read-only access for Savings Preview

Each file creates a `savings-preview` ServiceAccount in the `savings-preview` namespace and gives it only the read access one source needs. [docs/security.md](../../docs/security.md) lists every request behind these rules.

| File | For | Scope |
| --- | --- | --- |
| `poll.yaml` | `savings-preview poll` | ClusterRole |
| `prometheus.yaml` | `savings-preview prometheus`, with store discovery | ClusterRole |
| `prometheus-namespaced.yaml` | `savings-preview prometheus --target`, one store service | Role in the store's namespace |
| `poll-and-prometheus.yaml` | both sources | ClusterRole |
| `token-secret.yaml` | a token that does not expire, for long polls | Secret |

`poll` has no namespaced variant because it lists every namespace and the nodes. `prometheus-namespaced.yaml` needs two values filled in; the comments at its top explain them.

## Create the ServiceAccount

    kubectl create namespace savings-preview
    kubectl apply -f deploy/rbac/poll.yaml

## Make a kubeconfig for it

Run this with the kubeconfig context of the target cluster selected. It writes `savings-preview.kubeconfig`, which holds only the ServiceAccount's token.

    CLUSTER=$(kubectl config view --minify -o jsonpath='{.clusters[0].name}')
    SERVER=$(kubectl config view --minify -o jsonpath='{.clusters[0].cluster.server}')
    kubectl config view --minify --flatten \
      -o jsonpath='{.clusters[0].cluster.certificate-authority-data}' | base64 -d > ca.crt
    TOKEN=$(kubectl -n savings-preview create token savings-preview --duration 24h)

    export KC=savings-preview.kubeconfig
    kubectl --kubeconfig $KC config set-cluster "$CLUSTER" --server "$SERVER" \
      --certificate-authority ca.crt --embed-certs
    kubectl --kubeconfig $KC config set-credentials savings-preview --token "$TOKEN"
    kubectl --kubeconfig $KC config set-context savings-preview --cluster "$CLUSTER" --user savings-preview
    kubectl --kubeconfig $KC config use-context savings-preview
    rm ca.crt

    kubectl --kubeconfig $KC auth can-i --list
    savings-preview poll --kubeconfig $KC

The token from `kubectl create token` expires after `--duration`, and some clusters cap it lower. A `poll` that runs for days needs a token that outlives it. Apply `token-secret.yaml` and use its token instead:

    kubectl apply -f deploy/rbac/token-secret.yaml
    TOKEN=$(kubectl -n savings-preview get secret savings-preview-token -o jsonpath='{.data.token}' | base64 -d)

Deleting the Secret revokes that token.

## Remove it

    kubectl delete -f deploy/rbac/poll.yaml
    kubectl delete namespace savings-preview

Use the file you applied in place of `poll.yaml`.
