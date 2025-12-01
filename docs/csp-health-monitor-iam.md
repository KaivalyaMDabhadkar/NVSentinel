# CSP Health Monitor IAM Requirements

## Overview

The CSP Health Monitor requires specific Identity and Access Management (IAM) permissions to detect and process maintenance events from cloud service providers. This document outlines the required IAM permissions for each supported CSP.

The monitor uses Workload Identity (GCP) or IAM Roles for Service Accounts (IRSA) on AWS to securely authenticate and access cloud provider APIs without managing static credentials.

### Why Are These Permissions Needed?

The CSP Health Monitor needs IAM permissions to:

- **Detect maintenance events**: Query cloud provider APIs for scheduled and unscheduled maintenance
- **Identify affected resources**: Retrieve details about which compute instances are impacted
- **Get event details**: Access recommended remediation actions from the cloud provider
- **Map resources to nodes**: Query Kubernetes API to map cloud instance IDs to node names

## Google Cloud Platform (GCP)

### Required IAM Permissions

The CSP Health Monitor uses the GCP Cloud Logging API (`logadmin.Client.Entries()`) to poll for maintenance event logs. The following permission is required:

| Permission | Purpose |
|------------|---------|
| `logging.logEntries.list` | List log entries via the Cloud Logging `entries.list` API to detect maintenance events |

### Predefined Role

The required permission is included in the predefined role:

- **`roles/logging.viewer`** - Grants read-only access to Cloud Logging

### Workload Identity Setup

The Kubernetes ServiceAccount must be linked to a GCP Service Account via Workload Identity.

> **Note**: 
> - `<GCP_SA_NAME>` is the GCP Service Account name you choose (e.g., `csp-health-monitor`). This must match the value set in `configToml.gcp.gcpServiceAccountName`.
> - `<NAMESPACE>` is the Kubernetes namespace where NVSentinel is deployed (default: `nvsentinel`).
> - The Kubernetes ServiceAccount name is always `csp-health-monitor` (hardcoded in the Helm chart).

1. **Create a GCP Service Account** in the target project:

```bash
gcloud iam service-accounts create <GCP_SA_NAME> \
    --display-name="CSP Health Monitor Service Account" \
    --project=<TARGET_PROJECT_ID>
```

2. **Grant the logging.viewer role** to the GCP Service Account:

```bash
gcloud projects add-iam-policy-binding <TARGET_PROJECT_ID> \
    --member="serviceAccount:<GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com" \
    --role="roles/logging.viewer"
```

3. **Allow the Kubernetes ServiceAccount to impersonate the GCP Service Account**:

```bash
gcloud iam service-accounts add-iam-policy-binding \
    <GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com \
    --role="roles/iam.workloadIdentityUser" \
    --member="serviceAccount:<GKE_PROJECT_ID>.svc.id.goog[<NAMESPACE>/csp-health-monitor]"
```

4. **Configure Helm values** (the annotation is added automatically):

```yaml
configToml:
  gcp:
    targetProjectId: "<TARGET_PROJECT_ID>"
    gcpServiceAccountName: "<GCP_SA_NAME>"
```

The Helm chart automatically generates the ServiceAccount annotation:
```yaml
iam.gke.io/gcp-service-account: <GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com
```

### Custom IAM Policy (Least Privilege)

For minimal permissions, create a custom role with only the required permission:

```bash
gcloud iam roles create cspHealthMonitorRole \
    --project=<TARGET_PROJECT_ID> \
    --title="CSP Health Monitor Role" \
    --description="Minimal permissions for CSP Health Monitor" \
    --permissions="logging.logEntries.list"
```

Then assign this custom role to the GCP Service Account instead of `roles/logging.viewer`:

```bash
gcloud projects add-iam-policy-binding <TARGET_PROJECT_ID> \
    --member="serviceAccount:<GCP_SA_NAME>@<TARGET_PROJECT_ID>.iam.gserviceaccount.com" \
    --role="projects/<TARGET_PROJECT_ID>/roles/cspHealthMonitorRole"
```

## Amazon Web Services (AWS)

### Required IAM Permissions

The CSP Health Monitor uses the AWS Health API to poll for maintenance events. The following permissions are required:

| Permission | Purpose |
|------------|---------|
| `health:DescribeEvents` | Poll the AWS Health API for scheduled and unscheduled maintenance events |
| `health:DescribeAffectedEntities` | Retrieve specific EC2 instance IDs affected by maintenance events |
| `health:DescribeEventDetails` | Get detailed event information including recommended remediation actions |

### IAM Policy

Create an IAM policy with the required permissions:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Sid": "CSPHealthMonitorHealthAPIAccess",
            "Effect": "Allow",
            "Action": [
                "health:DescribeEvents",
                "health:DescribeAffectedEntities",
                "health:DescribeEventDetails"
            ],
            "Resource": "*"
        }
    ]
}
```

> **Note**: AWS Health API actions require `Resource: "*"` as they do not support resource-level permissions.

### IRSA Setup (IAM Roles for Service Accounts)

> **Note**: 
> - `<CLUSTER_NAME>` must match the value set in `configToml.clusterName`. This is used to construct the IAM role name.
> - `<NAMESPACE>` is the Kubernetes namespace where NVSentinel is deployed (default: `nvsentinel`).
> - The Kubernetes ServiceAccount name is always `csp-health-monitor` (hardcoded in the Helm chart).

1. **Ensure the OIDC provider is enabled** for your EKS cluster (typically already configured via Terraform).

2. **Create the IAM policy**:

```bash
aws iam create-policy \
    --policy-name CSPHealthMonitorPolicy \
    --policy-document '{
        "Version": "2012-10-17",
        "Statement": [
            {
                "Effect": "Allow",
                "Action": [
                    "health:DescribeEvents",
                    "health:DescribeAffectedEntities",
                    "health:DescribeEventDetails"
                ],
                "Resource": "*"
            }
        ]
    }'
```

3. **Create the IAM role with trust policy** for the EKS OIDC provider:

```bash
OIDC_PROVIDER=$(aws eks describe-cluster --name <CLUSTER_NAME> --query "cluster.identity.oidc.issuer" --output text | sed 's|https://||')
ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)

cat > trust-policy.json << EOF
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": {
                "Federated": "arn:aws:iam::${ACCOUNT_ID}:oidc-provider/${OIDC_PROVIDER}"
            },
            "Action": "sts:AssumeRoleWithWebIdentity",
            "Condition": {
                "StringEquals": {
                    "${OIDC_PROVIDER}:aud": "sts.amazonaws.com",
                    "${OIDC_PROVIDER}:sub": "system:serviceaccount:<NAMESPACE>:csp-health-monitor"
                }
            }
        }
    ]
}
EOF

aws iam create-role \
    --role-name <CLUSTER_NAME>-nvsentinel-health-monitor-assume-role-policy \
    --assume-role-policy-document file://trust-policy.json
```

4. **Attach the policy to the role**:

```bash
aws iam attach-role-policy \
    --role-name <CLUSTER_NAME>-nvsentinel-health-monitor-assume-role-policy \
    --policy-arn arn:aws:iam::${ACCOUNT_ID}:policy/CSPHealthMonitorPolicy
```

5. **Configure Helm values** (the annotation is added automatically):

```yaml
cspName: "aws"
configToml:
  clusterName: "<CLUSTER_NAME>"
  aws:
    accountId: "<ACCOUNT_ID>"
    region: "<AWS_REGION>"
    pollingIntervalSeconds: 60
```

The Helm chart automatically adds the annotation:
```yaml
eks.amazonaws.com/role-arn: arn:aws:iam::<ACCOUNT_ID>:role/<CLUSTER_NAME>-nvsentinel-health-monitor-assume-role-policy
```

## Azure (Coming Soon)

Azure support is planned for a future release. This section will be updated with the required Azure IAM permissions and Workload Identity configuration when available.

## Kubernetes RBAC

In addition to cloud provider IAM permissions, the CSP Health Monitor requires Kubernetes RBAC permissions to map cloud instance IDs to Kubernetes node names.

### Required Permissions

| Resource | Verbs | Purpose |
|----------|-------|---------|
| `nodes` | `get`, `list` | Query nodes to map cloud provider instance IDs to node names |
| `nodes/status` | `get`, `list` | Access node status information |

### ClusterRole

The Helm chart automatically creates the following ClusterRole:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: csp-health-monitor
rules:
- apiGroups:
  - ""
  resources:
  - nodes
  - nodes/status
  verbs:
  - get
  - list
```

## Helm Configuration

The CSP Health Monitor is deployed as part of the NVSentinel Helm chart. This section shows how to configure the monitor via Helm values.

> **Note**: The examples below use sample values (e.g., `my-gke-cluster`, `my-gcp-project-id`, `csp-health-monitor`). Replace these with your actual values. The GCP Service Account name in `gcpServiceAccountName` must match the GCP SA you created in the IAM setup section.

### Enabling CSP Health Monitor

Add the following to your custom values file:

```yaml
global:
  cspHealthMonitor:
    enabled: true
```

### GCP Configuration Example

Complete configuration for monitoring GCP Compute Engine maintenance events:

```yaml
global:
  cspHealthMonitor:
    enabled: true

csp-health-monitor:
  # Select GCP as the cloud provider
  cspName: "gcp"
  
  # Log level for debugging
  logLevel: info
  
  configToml:
    # Cluster identifier used in health events
    clusterName: "my-gke-cluster"
    
    # Quarantine trigger timing settings
    maintenanceEventPollIntervalSeconds: 60
    triggerQuarantineWorkflowTimeLimitMinutes: 30
    postMaintenanceHealthyDelayMinutes: 15
    nodeReadinessTimeoutMinutes: 60
    
    gcp:
      # GCP project ID where the cluster runs
      targetProjectId: "my-gcp-project-id"
      
      # GCP Service Account name (without @project.iam.gserviceaccount.com)
      # This is used to generate the Workload Identity annotation
      gcpServiceAccountName: "csp-health-monitor"
      
      # How often to poll Cloud Logging API (seconds)
      apiPollingIntervalSeconds: 60
      
      # Cloud Logging filter for maintenance events
      # Customize based on your needs
      logFilter: 'logName="projects/my-gcp-project-id/logs/cloudaudit.googleapis.com%2Fsystem_event" AND protoPayload.methodName="compute.instances.upcomingMaintenance"'
```

The Helm chart automatically generates the ServiceAccount annotation:

```yaml
iam.gke.io/gcp-service-account: csp-health-monitor@my-gcp-project-id.iam.gserviceaccount.com
```

### AWS Configuration Example

Complete configuration for monitoring AWS EC2 maintenance events:

```yaml
global:
  cspHealthMonitor:
    enabled: true

csp-health-monitor:
  # Select AWS as the cloud provider
  cspName: "aws"
  
  # Log level for debugging
  logLevel: info
  
  configToml:
    # Cluster identifier - IMPORTANT: This is used to construct the IAM role ARN
    clusterName: "my-eks-cluster"
    
    # Quarantine trigger timing settings
    maintenanceEventPollIntervalSeconds: 60
    triggerQuarantineWorkflowTimeLimitMinutes: 30
    postMaintenanceHealthyDelayMinutes: 15
    nodeReadinessTimeoutMinutes: 60
    
    aws:
      # AWS Account ID (12-digit number)
      accountId: "123456789012"
      
      # AWS region where the EKS cluster runs
      region: "us-east-1"
      
      # How often to poll AWS Health API (seconds)
      pollingIntervalSeconds: 60
```

The Helm chart automatically generates the ServiceAccount annotation:

```yaml
eks.amazonaws.com/role-arn: arn:aws:iam::123456789012:role/my-eks-cluster-nvsentinel-health-monitor-assume-role-policy
```

> **Important**: The IAM role name must match the pattern `<clusterName>-nvsentinel-health-monitor-assume-role-policy` where `<clusterName>` is the value from `configToml.clusterName`.

### Configuration Reference

| Value | Description | Default |
|-------|-------------|---------|
| `cspName` | Cloud provider: `"gcp"` or `"aws"` | `""` |
| `logLevel` | Log verbosity: `debug`, `info`, `warn`, `error` | `info` |
| `configToml.clusterName` | Cluster identifier for health events | `""` |
| `configToml.maintenanceEventPollIntervalSeconds` | How often sidecar polls MongoDB for events | `60` |
| `configToml.triggerQuarantineWorkflowTimeLimitMinutes` | Minutes before maintenance to trigger quarantine | `30` |
| `configToml.postMaintenanceHealthyDelayMinutes` | Minutes after maintenance to send healthy event | `15` |
| `configToml.nodeReadinessTimeoutMinutes` | Timeout for node readiness after maintenance | `60` |

**GCP-specific:**

| Value | Description | Default |
|-------|-------------|---------|
| `configToml.gcp.targetProjectId` | GCP project ID | `""` |
| `configToml.gcp.gcpServiceAccountName` | GCP SA name (without domain) | `""` |
| `configToml.gcp.apiPollingIntervalSeconds` | Cloud Logging poll interval | `60` |
| `configToml.gcp.logFilter` | Cloud Logging filter expression | `""` |

**AWS-specific:**

| Value | Description | Default |
|-------|-------------|---------|
| `configToml.aws.accountId` | AWS account ID (12 digits) | `""` |
| `configToml.aws.region` | AWS region | `""` |
| `configToml.aws.pollingIntervalSeconds` | Health API poll interval | `60` |

### Verifying Configuration

After deployment, verify the ServiceAccount has the correct annotation:

```bash
# For GCP
kubectl get serviceaccount csp-health-monitor -n nvsentinel -o jsonpath='{.metadata.annotations.iam\.gke\.io/gcp-service-account}'

# For AWS
kubectl get serviceaccount csp-health-monitor -n nvsentinel -o jsonpath='{.metadata.annotations.eks\.amazonaws\.com/role-arn}'
```

## Troubleshooting

### GCP Permission Errors

If you see errors like `PERMISSION_DENIED: The caller does not have permission`, verify:

1. The GCP Service Account has the `roles/logging.viewer` role
2. Workload Identity is properly configured
3. The Kubernetes ServiceAccount annotation matches the GCP Service Account email

Test permissions manually:
```bash
gcloud logging read "logName=\"projects/<PROJECT_ID>/logs/cloudaudit.googleapis.com%2Fsystem_event\"" \
    --project=<PROJECT_ID> \
    --limit=1 \
    --impersonate-service-account=<GCP_SA_NAME>@<PROJECT_ID>.iam.gserviceaccount.com
```

### AWS Permission Errors

If you see `AccessDeniedException` errors, verify:

1. The IAM policy is attached to the role
2. The OIDC trust policy has the correct cluster OIDC provider URL
3. The ServiceAccount annotation matches the IAM role ARN

Test permissions manually:
```bash
aws health describe-events --filter "services=EC2" --max-items 1
```

### Node Mapping Failures

If the monitor cannot map cloud instance IDs to Kubernetes nodes:

1. Verify the pod has RBAC permissions to list nodes
2. Check that nodes have the correct `spec.providerID` set
3. Ensure the monitor is running in the correct cluster context

## References

- [GCP Cloud Logging IAM Permissions](https://cloud.google.com/logging/docs/access-control)
- [GCP Workload Identity](https://cloud.google.com/kubernetes-engine/docs/how-to/workload-identity)
- [AWS Health API IAM Permissions](https://docs.aws.amazon.com/health/latest/ug/security_iam_service-with-iam.html)
- [AWS IAM Roles for Service Accounts (IRSA)](https://docs.aws.amazon.com/eks/latest/userguide/iam-roles-for-service-accounts.html)

