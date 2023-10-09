[![Go Report Card](https://goreportcard.com/badge/github.com/costela/hcloud-ip-floater)](https://goreportcard.com/report/github.com/costela/hcloud-ip-floater)
[![Build Status](https://github.com/costela/hcloud-ip-floater/actions/workflows/main.yaml/badge.svg)](https://github.com/costela/hcloud-ip-floater/actions/workflows/release.yaml)

# Hetzner Cloud™ IP Floater

This small [kubernetes](https://kubernetes.io/) controller manages the attachment of
[hetzner cloud](https://hetzner.cloud) ("hcloud") floating IPs to kubernetes nodes.

It watches for changes to kubernetes `LoadBalancer` services, chooses one of the nodes where its pods are scheduled and
attaches its assigned floating IP to the selected node.

The service IP assignment is left to a separate component, like [MetalLB](https://metallb.universe.tf/).

## Installation

The controller can be installed to a cluster using e.g. [kustomize](https://kustomize.io/). Simply `kubectl apply -k` the
following `kustomization.yaml`:

```yaml
namespace: hcloud-ip-floater
bases:
  - github.com/costela/hcloud-ip-floater/deploy?ref=v0.1.6
secretGenerator:
  - name: hcloud-ip-floater-secret-env
    literals:
      - HCLOUD_IP_FLOATER_HCLOUD_TOKEN=<YOUR HCLOUD API TOKEN HERE>
```

The provided deployment manifest expects a secret named `hcloud-ip-floater-secret-env` to exist, which is the
recommended location for storing the hcloud API token.

It's also possible to provide a `configMapGenerator` called `hcloud-ip-floater-config-env` with the non-secret options
listed in the [configuration options](#configuration-options) section below.

⚠ in order for the controller to attach IPs to the hcloud nodes, the k8s nodes **must** use the same names as in
hcloud.

## Configuration options

Either as command line arguments or environment variables.

### `--hcloud-token` or `HCLOUD_IP_FLOATER_HCLOUD_TOKEN` **(required)**

API token for hetzner cloud access.

### `--service-label-selector` or `HCLOUD_IP_FLOATER_SERVICE_LABEL_SELECTOR` 

Service label selector to use when watching for kubernetes services. Any services that do not match this selector will be ignored by the controller.

**Default**: `hcloud-ip-floater.cstl.dev/ignore!=true`

### `--floating-label-selector` or `HCLOUD_IP_FLOATER_FLOATING_LABEL_SELECTOR`

Label selector for hcloud floating IPs. Floating IPs that do not match this selector will be ignored by the controller. 

**Default**: `hcloud-ip-floater.cstl.dev/ignore!=true`

### `--log-level` or `HCLOUD_IP_FLOATER_LOG_LEVEL`

Log output verbosity (debug/info/warn/error)

**Default**: `warn`
