# Kubernetes manifests

- `agent/agent.yaml`: the read-only Clusterheart agent (observe-only RBAC). The
  dashboard's **Add cluster** flow gives you the exact install command.
- `remediation/remediation.yaml`: the opt-in remediation controller, which can
  only `get`/`update` Deployments, StatefulSets and DaemonSets. The server
  serves a copy at `/remediation.yaml` (`server/remediation.yaml`, kept in sync
  by `server/app_apply_test.go`).
