# Deploy Kubernetes (Helm)

This guide covers running Nenya on Kubernetes using the Helm chart in `deploy/chart/nenya/`.

## Prerequisites

- Kubernetes 1.19+
- Helm 3.8+
- kubectl configured with cluster access

## Helm Chart Overview

The Helm chart provides:

| Component | Description |
|-----------|-------------|
| Deployment | Security-hardened with non-root user, read-only fs, tmpfs |
| Service | ClusterIP (default) |
| ConfigMap | Optional: inline config files |
| Secret | Optional: inline secrets (stringData) |
| Ingress | Optional: ingress with TLS |

Key security features:
- Non-root user (UID 65532)
- Read-only root filesystem
- Dropped capabilities (ALL), only IPC_LOCK added
- tmpfs mounted at `/tmp`

## Create Namespaces

```bash
kubectl create namespace nenya
# Or use an existing namespace
```

## Create Secrets

Create a Kubernetes Secret for provider keys and client token:

```bash
# Create secret from individual files
kubectl create secret generic nenya-secrets \
  --from-file=provider_keys.json=secrets/provider_keys.json \
  --from-file=client.json=secrets/client.json \
  -n nenya

# Or create from literal values
kubectl create secret generic nenya-secrets \
  --from-literal=provider_keys.json='{"provider_keys":{"gemini":"AIza..."}}' \
  --from-literal=client.json='{"client_token":"nk-..."}' \
  -n nenya
```

## Create ConfigMap

Create a ConfigMap for configuration files:

```bash
# Create from config directory
kubectl create configmap nenya-config \
  --from-file=config.json=config/config.json \
  -n nenya

# Or from literal
kubectl create configmap nenya-config \
  --from-literal=config.json='{"server":{"listen_addr":":8080"},"agents":{"default":{"strategy":"fallback","models":["gemini-2.5-flash"]}}}' \
  -n nenya
```

## Install with Helm

```bash
# Add the chart (if hosted), or use local path
helm install nenya ./deploy/chart/nenya \
  --namespace nenya \
  --set secrets.existingSecret=nenya-secrets \
  --set config.existingConfigMap=nenya-config
```

### Using a different release name

```bash
helm install my-nenya ./deploy/chart/nenya \
  --namespace nenya \
  --release-name my-nenya
```

### Chart options

```bash
# View all configurable options
helm show values ./deploy/chart/nenya

# Override specific values
helm install nenya ./deploy/chart/nenya \
  --namespace nenya \
  --set secrets.existingSecret=nenya-secrets \
  --set config.existingConfigMap=nenya-config \
  --set image.tag=v0.2.0 \
  --set replicaCount=2 \
  --set service.type=LoadBalancer
```

## Verify Installation

Check the deployment status:

```bash
kubectl get pods -n nenya
kubectl get svc -n nenya
```

View pod logs:

```bash
kubectl logs -n nenya -l app.kubernetes.io/name=nenya
```

Test the health endpoint via port-forward:

```bash
# Port forward to access the service
kubectl port-forward -n nenya svc/nenya 8080:8080

# In another terminal
curl http://localhost:8080/healthz
```

## Configuration Examples

### Basic configuration

```yaml
# values.yaml
replicaCount: 1

image:
  repository: ghcr.io/gumieri/nenya
  pullPolicy: IfNotPresent
  tag: "latest"

secrets:
  existingSecret: nenya-secrets

config:
  existingConfigMap: nenya-config

service:
  type: ClusterIP
  port: 8080
```

### High availability

```yaml
# values.yaml
replicaCount: 3

service:
  type: LoadBalancer

topologySpreadConstraints:
  - maxSkew: 1
    topologyKey: topology.kubernetes.io/zone
    whenUnsatisfiable: ScheduleAnyway
    labelSelector:
      matchLabels:
        app.kubernetes.io/name: nenya
```

### With ingress

```yaml
# values.yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt-prod
  hosts:
    - host: nenya.example.com
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: nenya-tls
      hosts:
        - nenya.example.com
```

Install with custom values:

```bash
helm install nenya ./deploy/chart/nenya \
  --namespace nenya \
  --values values.yaml \
  --set secrets.existingSecret=nenya-secrets \
  --set config.existingConfigMap=nenya-config
```

## Raw Manifests (Non-Helm)

If you prefer raw Kubernetes manifests without Helm, generate them:

```bash
# Generate manifests without installing
helm template nenya ./deploy/chart/nenya \
  --namespace nenya \
  --set secrets.existingSecret=nenya-secrets \
  --set config.existingConfigMap=nenya-config

# Save to file
helm template nenya ./deploy/chart/nenya \
  --namespace nenya \
  --set secrets.existingSecret=nenya-secrets \
  --set config.existingConfigMap=nenya-config \
  > nenya-manifests.yaml

# Apply directly
kubectl apply -f nenya-manifests.yaml
```

### Manual manifest (minimal)

For quick testing, here's a minimal manifest:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: nenya
  namespace: nenya
spec:
  type: ClusterIP
  ports:
    - port: 8080
      targetPort: 8080
  selector:
    app.kubernetes.io/name: nenya
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: nenya
  namespace: nenya
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: nenya
  template:
    metadata:
      labels:
        app.kubernetes.io/name: nenya
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        fsGroup: 65532
      containers:
        - name: nenya
          image: ghcr.io/gumieri/nenya:latest
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities:
              drop: [ALL]
              add: [IPC_LOCK]
          ports:
            - containerPort: 8080
          env:
            - name: NENYA_SECRETS_DIR
              value: /run/secrets/nenya
          volumeMounts:
            - name: config
              mountPath: /etc/nenya
              readOnly: true
            - name: secrets
              mountPath: /run/secrets/nenya
              readOnly: true
            - name: tmp
              mountPath: /tmp
          livenessProbe:
            httpGet:
              path: /healthz
              port: 8080
          readinessProbe:
            httpGet:
              path: /healthz
              port: 8080
          resources: {}
      volumes:
        - name: config
          configMap:
            name: nenya-config
        - name: secrets
          secret:
            secretName: nenya-secrets
        - name: tmp
          emptyDir:
            medium: Memory
            sizeLimit: 64Mi
```

Apply:

```bash
kubectl apply -f nenya-minimal.yaml -n nenya
```

## Upgrades

Upgrade to a new version:

```bash
helm upgrade nenya ./deploy/chart/nenya \
  --namespace nenya \
  --set secrets.existingSecret=nenya-secrets \
  --set config.existingConfigMap=nenya-config
```

## Troubleshooting

### Pod not starting

Check pod events:

```bash
kubectl describe pod -n nenya -l app.kubernetes.io/name=nenya
```

### Health check failing

```bash
kubectl logs -n nenya -l app.kubernetes.io/name=nenya
```

### Config not loading

Verify ConfigMap contents:

```bash
kubectl get configmap nenya-config -n nenya -o yaml
```

### Secrets not accessible

Verify Secret exists:

```bash
kubectl get secret nenya-secrets -n nenya -o yaml
```

### Image pull issues

Check image pull policy and registry access:

```bash
kubectl describe pod -n nenya -l app.kubernetes.io/name=nenya | grep -A 10 "Events:"
```

## Uninstall

```bash
helm uninstall nenya -n nenya

# Clean up resources (if not managed by Helm)
kubectl delete secret nenya-secrets -n nenya
kubectl delete configmap nenya-config -n nenya
kubectl delete service nenya -n nenya
```
