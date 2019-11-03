# Hetzner Cloud™ IP Floater

This small [kubernetes](https://kubernetes.io/) controller manages the attachment of
[hetzner cloud](https://hetzner.cloud) ("hcloud") floating IPs to kubernetes nodes.

It watches for changes to kubernetes `LoadBalancer` services, chooses one of the nodes where its pods are scheduled and
attaches its assigned floating IP to the selected node.

The service IP assignment is left to a separate controller, like [MetalLB](https://metallb.universe.tf/).

## Installation

The controller can be installed to a cluster using [kustomize](https://kustomize.io/). Simply `kubectl apply -k` the
following `kustomization.yaml`:

```yaml
namespace: hcloud-ip-floater
bases:
  - github.com/costela/hcloud-ip-floater/deploy?ref=v0.1.1
secretGenerator:
  - name: hcloud-ip-floater-secret-env
    literals:
      - HCLOUD_IP_FLOATER_HCLOUD_TOKEN=<YOUR HCLOUD API TOKEN HERE>
```

The provided deployment manifest expects a secret named `hcloud-ip-floater-secret-env` to exist, which is the
recommended location for storing the hcloud API token.

It's also possible to provide a `configMapGenerator` called `hcloud-ip-floater-config-env` with the non-secret options
listed in the [configuration options](#configuration-options) section below.

## Configuration options

| Option | Env | Description | Default |
|---|---|---|---|
| `--hcloud-token` | HCLOUD_IP_FLOATER_HCLOUD_TOKEN | API token for hetzner cloud access | |
| `--service-label-selector` | HCLOUD_IP_FLOATER_SERVICE_LABEL_SELECTOR | service label selector to use when watching for kubernetes services | `hcloud-ip-floater.cstl.dev/ignore!=true` |
| `--floating-label-selector` | HCLOUD_IP_FLOATER_FLOATING_LABEL_SELECTOR | label selector for hcloud floating IPs; only matching IPs will be available to the controller | `hcloud-ip-floater.cstl.dev/ignore!=true` |
| `--log-level` | HCLOUD_IP_FLOATER_LOG_LEVEL | Log output verbosity (debug/info/warn/error) | `warn` |
