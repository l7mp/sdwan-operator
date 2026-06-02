# SD-WAN operator: Cilium Mesh -- SD-WAN integration

The SD-WAN operator is a simple Kubernetes controller that enforces Cilium Mesh traffic management policies across a Cisco SD-WAN network substrate. The operator uses the [Δ-controller](https://github.com/l7mp/dcontroller) Kubernetes operator framework.

![Cilium Mesh -- SD-WAN integration architecture](/img/sdwan-cilium-arch.png)

## Quick Start

### Prerequisites

- A Kubernetes cluster (v1.28+)
- `kubectl` access to the cluster
- A Cisco vManage instance with API access credentials
- `helm` v3+

### Configure vManage credentials

Create a `values.yaml` with your vManage connection details:

```yaml
vmanage:
  url: https://<vmanage-host>:443
  username: admin
  password: <your-password>
  insecure: false
```

### Install from Helm repository

```bash
helm repo add sdwan-operator https://l7mp.github.io/sdwan-operator/
helm repo update
helm install sdwan-operator sdwan-operator/sdwan-operator \
  --namespace sdwan-operator \
  --create-namespace \
  -f values.yaml
```

### Verify

```bash
kubectl get pods -n sdwan-operator
```

## Blog Post

This project was featured in the CNCF blog post [Connecting distributed Kubernetes with Cilium and SD-WAN: Building an intelligent network fabric](https://www.cncf.io/blog/2025/10/25/connecting-distributed-kubernetes-with-cilium-and-sd-wan-building-an-intelligent-network-fabric/).

## License

Copyright 2025-2026 by its authors. Some rights reserved. See [AUTHORS](AUTHORS).

Apache License - see [LICENSE](LICENSE) for full text.
