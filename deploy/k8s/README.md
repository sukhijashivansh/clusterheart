# Clusterheart on the web (in-cluster deploy)

The desktop app runs the dashboard on your laptop against your **local**
kubeconfig. This deploys the *same* Go server **inside a cluster** instead: it
runs as a pod, reads the cluster through a ServiceAccount + RBAC (no kubeconfig,
no credentials leave anywhere), and serves the UI to a browser.

> ⚠️ **No authentication.** These manifests put the dashboard behind no login.
> Keep it private — reach it via `kubectl port-forward` or a VPN/internal
> LoadBalancer only. Don't expose it to the public internet as-is.

## 1. Build the image

```bash
# from the repo root
docker build -t clusterheart:latest app/
```

Push it to a registry your cluster can pull from and update `image:` in
`clusterheart.yaml`, e.g.:

```bash
docker build -t <registry>/clusterheart:latest app/
docker push <registry>/clusterheart:latest
# then set image: <registry>/clusterheart:latest in clusterheart.yaml
```

On a local cluster you can skip the registry and load the image directly:

```bash
kind load docker-image clusterheart:latest          # kind
minikube image load clusterheart:latest             # minikube
```

## 2. Deploy

```bash
kubectl apply -f deploy/k8s/clusterheart.yaml
kubectl apply -f deploy/k8s/mongo.yaml   # optional — rightsizing history
kubectl -n clusterheart rollout status deploy/clusterheart
```

## 3. Open it

```bash
kubectl -n clusterheart port-forward svc/clusterheart 8088:80
open http://localhost:8088
```

For a real URL on a cloud cluster, change the Service in `clusterheart.yaml` to
`type: LoadBalancer` (ideally an *internal* LB), or front it with an Ingress —
add your own auth (OAuth2 proxy / basic auth) before exposing it more widely.

## What works in-cluster vs. desktop

| Feature | In-cluster | Notes |
|---|---|---|
| Pods / nodes / jobs / HPAs / stats | ✅ | via the ServiceAccount |
| Logs, YAML, diagnose, shell, port-forward, load test | ✅ | `kubectl` is bundled in the image; auths via the mounted token |
| Rightsizing history | ✅ | needs `mongo.yaml` (or point `MONGO_URI` at your own Mongo) |
| Context switcher | ➖ | one cluster only (the one it lives in) — the dropdown is empty |
| AWS cost panel (FinOps) | ➖ | needs the `aws` CLI + credentials; omitted from the image. To enable, give the pod IAM via IRSA and add `aws` to the Dockerfile |
| AI chat | ✅ | set the provider/key in the AI tab as usual |

## Trimming RBAC to read-only

`clusterheart.yaml`'s ClusterRole includes a block of mutating verbs (delete pod,
cleanse namespace, rightsizing/scale patches). Delete that block for a
read-only deployment; those buttons will then return 403.
