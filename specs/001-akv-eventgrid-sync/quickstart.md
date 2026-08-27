# Quickstart & Validation Guide

Proves the feature end-to-end: declare a sync, see the Secret appear, rotate in
Key Vault, watch the update land in under 60 seconds, then verify the safety
net. References: [data-model.md](data-model.md), [contracts/](contracts/).

## Prerequisites

- An AKS cluster with the OIDC issuer and workload identity enabled
  (`az aks update --enable-oidc-issuer --enable-workload-identity`), and
  `kubectl` access to it.
- An Azure Key Vault (RBAC permission model) and a Storage Account.
- Azure CLI logged in with rights to create role assignments, an Event Grid
  subscription, and a user-assigned managed identity.

## 1. Azure-side setup (one time, operator's responsibility per spec)

```bash
RG=<resource-group> VAULT=<vault-name> SA=<storage-account> QUEUE=kvsynk8s-events
AKS=<cluster-name> NS=kvsynk8s SA_K8S=kvsynk8s-controller-manager

# Queue that receives Key Vault events
az storage queue create --name $QUEUE --account-name $SA --auth-mode login

# Route Key Vault events to the queue (Event Grid system topic)
az eventgrid event-subscription create \
  --name kvsynk8s \
  --source-resource-id $(az keyvault show -n $VAULT -g $RG --query id -o tsv) \
  --endpoint-type storagequeue \
  --endpoint $(az storage account show -n $SA -g $RG --query id -o tsv)/queueservices/default/queues/$QUEUE \
  --included-event-types Microsoft.KeyVault.SecretNewVersionCreated

# Identity for the operator + federation with its ServiceAccount.
# CLIENT_ID is needed again in step 2 to annotate the ServiceAccount.
az identity create -n kvsynk8s-operator -g $RG
CLIENT_ID=$(az identity show -n kvsynk8s-operator -g $RG --query clientId -o tsv)
ISSUER=$(az aks show -n $AKS -g $RG --query oidcIssuerProfile.issuerUrl -o tsv)
az identity federated-credential create -n kvsynk8s -g $RG \
  --identity-name kvsynk8s-operator \
  --issuer $ISSUER --subject system:serviceaccount:$NS:$SA_K8S

# Least-privilege roles (constitution V)
PRINCIPAL=$(az identity show -n kvsynk8s-operator -g $RG --query principalId -o tsv)
az role assignment create --assignee $PRINCIPAL --role "Key Vault Secrets User" \
  --scope $(az keyvault show -n $VAULT -g $RG --query id -o tsv)
az role assignment create --assignee $PRINCIPAL --role "Storage Queue Data Message Processor" \
  --scope "$(az storage account show -n $SA -g $RG --query id -o tsv)/queueServices/default/queues/$QUEUE"
```

## 2. Install the operator

Install from the release manifest. It is the same CRD + RBAC + Deployment the
kustomize tree produces, but with the image already pinned to that release's
tag, so the pod can actually start:

```bash
kubectl apply -f https://github.com/tabman83/kvsynk8s/releases/download/v0.1.0/install.yaml
```

Do **not** install with a bare `kubectl apply -k config/default`. Nothing under
`config/` substitutes the image: the Deployment comes out with the
`controller:latest` placeholder, which does not exist in any registry, so the
pod goes straight to `ImagePullBackOff` and no scenario below can pass. Only
`make deploy` (or the release manifest) pins a real image.

To validate code that is not released yet, build and push your own image and
deploy from source instead. Note `make deploy` runs
`kustomize edit set image`, so it leaves `config/manager/kustomization.yaml`
modified in your checkout — revert it when you are done:

```bash
make docker-build docker-push IMG=<your-registry>/kvsynk8s:dev
make deploy IMG=<your-registry>/kvsynk8s:dev
```

Either way the install carries the workload-identity pod label and a real
image, but NOT the client ID or the queue URL: the ServiceAccount ships a
`<SET-ME>` placeholder annotation and the manager has no queue configured. The
two commands below fill both in — without them workload identity cannot
authenticate and the near-realtime path (V2) never activates.

```bash
# (a) point Microsoft Entra Workload ID at the managed identity from step 1
kubectl -n $NS annotate serviceaccount $SA_K8S \
  azure.workload.identity/client-id=$CLIENT_ID --overwrite

# (b) hand the manager the queue URL so the listener starts.
# This changes the pod template, so it also rolls the Deployment and the new
# pod picks up the annotation from (a) at the same time.
kubectl -n $NS set env deploy/kvsynk8s-operator \
  QUEUE_URL="https://$SA.queue.core.windows.net/$QUEUE"

kubectl -n $NS rollout status deploy/kvsynk8s-operator --timeout=180s
```

## 3. Validation scenarios

### V1 — initial sync (User Story 1 / SC-002)

```bash
az keyvault secret set --vault-name $VAULT --name demo-password --value 'first-value'
kubectl create namespace demo
cat <<EOF | kubectl apply -f -
apiVersion: kvsynk8s.io/v1alpha1
kind: SecretSync
metadata:
  name: demo-password
  namespace: demo
spec:
  vault:
    name: $VAULT
    secret: demo-password
EOF
kubectl -n demo get secretsync demo-password        # expect State: InSync
kubectl -n demo get secret demo-password -o jsonpath='{.data.demo-password}' | base64 -d
# expect: first-value
```

### V2 — near-realtime rotation (User Story 2 / SC-001)

```bash
date +%T; az keyvault secret set --vault-name $VAULT --name demo-password --value 'second-value'
# poll until the value flips; must be < 60 s after the command above
watch -n 2 "kubectl -n demo get secret demo-password -o jsonpath='{.data.demo-password}' | base64 -d"
```

### V3 — recovery from missed events (User Story 3 / SC-003)

```bash
kubectl -n kvsynk8s scale deploy/kvsynk8s-operator --replicas=0
az keyvault secret set --vault-name $VAULT --name demo-password --value 'third-value'
az storage message clear --queue-name $QUEUE --account-name $SA --auth-mode login   # simulate lost event
kubectl -n kvsynk8s scale deploy/kvsynk8s-operator --replicas=1
# startup reconciliation converges without any event:
kubectl -n demo get secret demo-password -o jsonpath='{.data.demo-password}' | base64 -d
# expect: third-value (within startup reconciliation; worst case one interval, default 4 h)
```

### V4 — drift repair (User Story 3, scenario 2)

```bash
kubectl -n demo delete secret demo-password
# recreated by the controller (watch-triggered or within one reconciliation):
kubectl -n demo get secret demo-password
```

### V5 — failure isolation and status (User Story 4 / SC-006)

```bash
cat <<EOF | kubectl apply -f -
apiVersion: kvsynk8s.io/v1alpha1
kind: SecretSync
metadata: {name: broken, namespace: demo}
spec:
  vault: {name: $VAULT, secret: does-not-exist}
EOF
kubectl -n demo get secretsync
# expect: broken → Failing, reason SecretNotFound; demo-password stays InSync
```

### V6 — no value leaks (SC-004)

```bash
kubectl -n kvsynk8s logs deploy/kvsynk8s-operator | grep -c 'second-value\|third-value'
kubectl -n demo get secretsync -o yaml | grep -c 'second-value\|third-value'
# expect: 0 and 0
```

### V7 — cleanup follows the declaration (User Story 1, scenario 3)

```bash
kubectl -n demo delete secretsync demo-password
kubectl -n demo get secret demo-password   # expect: NotFound
```

## Automated tests

Three layers, all runnable without an Azure subscription (Docker required for
the last two):

- `make test` — unit + envtest: sync engine idempotency, event parsing per
  [contracts/queue-message.md](contracts/queue-message.md), backoff, ownership
  rules, reconciler/finalizer behavior, sentinel-value redaction.
- `make test-integration` — azqueue against Azurite and azsecrets against a
  Key Vault emulator via testcontainers-go.
- `make test-e2e` — full loop on a kind cluster: deploy, sync, queue event →
  Secret update <60 s, drift repair, cleanup.

CI runs all three on every PR. The manual validation V1–V7 above remains the
only check of real Event Grid delivery and workload identity. The exact
commands, including how to run a single test, are in CLAUDE.md's
"Build, lint, test" section.
