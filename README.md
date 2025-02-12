# Ingress Cloudflare DNS Controller

A Kubernetes controller that automatically updates Cloudflare DNS records to point to the node's public IP where the ingress backend service's first pod is running.

## ✨ Features

🔄 **Auto Sync**: Automatically synchronizes Ingress DNS records with node public IPs
🎯 **Flexible Filtering**: Support regex patterns to filter namespaces and domains
🔒 **Cloudflare Proxy**: Optional Cloudflare proxy (orange cloud) support
⚡️ **Real-time Updates**: Only updates DNS records when IP changes
🎮 **Easy Configuration**: Simple configuration through environment variables
🔍 **Smart Selection**: Intelligent pod and service selection for DNS routing

## Prerequisites

- Kubernetes cluster
- Cloudflare API token with DNS edit permissions
- Nodes must have the annotation `node.kubernetes.io/public-ip` set with their public IP address

## Configuration

The controller is configured using environment variables:

| Variable | Description | Default | Required |
|----------|-------------|---------|----------|
| CF_TOKEN | Cloudflare API token | - | Yes |
| CF_ZONE_ID | Cloudflare Zone ID | - | Yes |
| SYNC_INTERVAL_SECONDS | Interval between DNS sync operations | 300s | No |
| NODE_PUBLIC_IP_ANNOTATION | Node annotation key for public IP | node.kubernetes.io/public-ip | No |
| KUBECONFIG | Path to kubeconfig file | - | No |
| DNS_PROXIED | Whether to enable Cloudflare's proxy (orange cloud) | true | No |
| NAMESPACE_REGEX | Regex pattern to filter namespaces | .* | No |
| DOMAIN_REGEX | Regex pattern to filter domain names | .* | No |

Duration values (like SYNC_INTERVAL_SECONDS) support time units (s, m, h).
Example: "5m" for 5 minutes, "1h" for 1 hour.

### Filtering Examples

Filter namespaces and domains using regular expressions:

```bash
# Only process ingresses in namespaces starting with 'prod-'
export NAMESPACE_REGEX="^prod-.*"

# Only process domains ending with example.com
export DOMAIN_REGEX=".*\\.example\\.com$"

# Process multiple domains
export DOMAIN_REGEX=".*\\.(example\\.com|example\\.org)$"

# Process specific namespaces
export NAMESPACE_REGEX="^(prod|staging)$"
```

## Installation

1. Build the container image (Optional, if you want to build it yourself):
```bash
docker build -t ghcr.io/monlor/ingress-cf-dns:main .
```

2. Create the required Kubernetes resources:

First, encode your Cloudflare credentials:
```bash
echo -n "your-cloudflare-api-token" | base64
echo -n "your-cloudflare-zone-id" | base64
```

Update the Secret in `deploy/deployment.yaml` with your base64-encoded credentials:
```yaml
data:
  cf-token: <base64-encoded-token>
  cf-zone-id: <base64-encoded-zone-id>
```

3. Apply the Kubernetes manifests:
```bash
kubectl apply -f deploy/deployment.yaml
```

## How it works

1. The controller periodically scans all ingresses in the cluster (default: every 5 minutes)
2. For each ingress, it:
   - Checks if the ingress namespace matches the NAMESPACE_REGEX pattern
   - Checks if the ingress hosts match the DOMAIN_REGEX pattern
   - Gets the first path's backend service
   - Gets the first pod of that service
   - Gets the node where the pod is running
   - Retrieves the public IP from the node's annotation
   - Updates Cloudflare DNS record for the ingress host to point to the node's public IP
   - Only updates the DNS record if the IP address has changed
   - Respects the configured proxy setting (orange/gray cloud) via DNS_PROXIED

## Node Configuration

Ensure your nodes have the required annotation:
```bash
kubectl annotate node <node-name> node.kubernetes.io/public-ip=<public-ip-address>
```

## Limitations

- Each node must have its public IP configured as an annotation
- Only the first path's backend service of each ingress rule is considered
- Only the first pod of each service is used for DNS record
- One-to-one mapping between hosts and backends is enforced 