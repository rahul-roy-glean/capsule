NOTE: Replace placeholder values (YOUR_*, your-*) with your actual configuration.

Deploy bazel-firecracker on your-project-id

Phase 1: Local prerequisites

# Verify tools
go version          # >= 1.24
terraform version   # >= 1.0
packer version      # >= 1.9
gcloud version
kubectl version --client
helm version

# Auth
gcloud auth login
gcloud auth application-default login
gcloud config set project your-project-id

# Pull latest code (with all our fixes)
cd $PROJECT_ROOT
git pull origin main

# Verify everything builds
make build

Phase 2: Terraform state bucket

gcloud storage buckets create gs://your-bucket-name-tfstate \
--project=your-project-id --location=us-central1 --uniform-bucket-level-access

Phase 3: Store GitHub App private key

# Upload your existing GitHub App private key
gcloud secrets create github-app-key \
--project=your-project-id \
--data-file=/path/to/your-github-app-private-key.pem

Phase 4: Terraform

export DB_PASSWORD=$(openssl rand -base64 24)
echo "SAVE THIS: $DB_PASSWORD"

cd deploy/terraform

terraform init \
-backend-config="bucket=your-bucket-name-tfstate" \
-backend-config="prefix=firecracker-bazel-runner"

terraform plan \
-var="project_id=your-project-id" \
-var="db_password=$DB_PASSWORD" \
-var="github_runner_enabled=true" \
-var="github_repo=your-org/your-repo" \
-var="github_org=your-org" \
-var="github_app_id=YOUR_GITHUB_APP_ID" \
-var="github_app_secret=projects/your-project-id/secrets/github-app-key/versions/latest" \
-var="github_runner_labels=self-hosted,firecracker,Linux,X64,bazel" \
-var="git_cache_enabled=true" \
-var='git_cache_repos={"github.com/your-org/your-repo":"your-repo"}' \
-out=tfplan

terraform apply tfplan

This creates: VPC, subnets, Cloud NAT, Cloud Router, firewall rules, GKE cluster, Cloud SQL, GCS bucket, Artifact Registry, service accounts with IAM (including Secret Manager access), and an empty
MIG.

Phase 5: Database schema

# Get Cloud SQL instance name
cd deploy/terraform
SQL_INSTANCE=$(terraform output -raw sql_instance_name)
SQL_IP=$(terraform output -raw sql_private_ip)

# Option A: Cloud SQL Auth Proxy (from your local machine)
cloud-sql-proxy "your-project-id:us-central1:${SQL_INSTANCE}" &
psql "host=127.0.0.1 port=5432 user=postgres password=$DB_PASSWORD dbname=postgres" \
-f ../database/schema.sql

# Option B: From a GCE VM in the same VPC
# gcloud compute ssh ANY_VM_IN_VPC -- \
#   psql "host=$SQL_IP port=5432 user=postgres password=$DB_PASSWORD dbname=postgres" \
#   -f schema.sql

Phase 6: Build and push control plane container

cd $PROJECT_ROOT

# Configure Docker for Artifact Registry
gcloud auth configure-docker us-central1-docker.pkg.dev

# Build (--platform linux/amd64 is already in the Makefile)
make docker-build PROJECT_ID=your-project-id
make docker-push PROJECT_ID=your-project-id

Phase 7: Deploy control plane to GKE

# Get GKE credentials
GKE_CLUSTER=$(terraform -chdir=deploy/terraform output -raw gke_cluster_name)
gcloud container clusters get-credentials "$GKE_CLUSTER" \
--region=us-central1 --project=your-project-id

# Deploy via Helm (override the hardcoded your-project-id values explicitly)
SQL_IP=$(terraform -chdir=deploy/terraform output -raw sql_private_ip)

helm upgrade --install control-plane deploy/helm/firecracker-runner/ \
--set image.repository=us-central1-docker.pkg.dev/your-project-id/firecracker/control-plane \
--set image.tag=latest \
--set config.dbHost="$SQL_IP" \
--set config.dbPassword="$DB_PASSWORD" \
--set config.gcsBucket=your-bucket-name \
--set config.gcpProject=your-project-id

# Verify
kubectl get pods
kubectl logs -l app=control-plane --tail=20

Phase 8: Build microVM rootfs

cd $PROJECT_ROOT

# This uses Docker, works on macOS
make rootfs

# Verify output
ls -lh images/microvm/output/
# Should have: rootfs.img (~2-4GB), kernel.bin (~21MB)

Phase 9: Build Packer host image

# Cross-compiles firecracker-manager for linux/amd64, then runs Packer
make release-host-image PROJECT_ID=your-project-id

# Verify image was created
gcloud compute images list --project=your-project-id \
--filter="family:firecracker-host" --format="table(name,creationTimestamp)"

Phase 10: Create initial Firecracker snapshot

This must run on a Linux VM with KVM (nested virtualization).

# Create a temporary builder VM
gcloud compute instances create snapshot-builder-vm \
--project=your-project-id \
--zone=us-central1-a \
--machine-type=n2-standard-8 \
--image-family=ubuntu-2204-lts \
--image-project=ubuntu-os-cloud \
--boot-disk-size=100GB \
--enable-nested-virtualization \
--scopes=cloud-platform \
--network=$(terraform -chdir=deploy/terraform output -raw vpc_name) \
--subnet=$(terraform -chdir=deploy/terraform output -raw hosts_subnet_name)

# SSH in
gcloud compute ssh snapshot-builder-vm --zone=us-central1-a --project=your-project-id

On the VM:
# Install Go
sudo apt-get update && sudo apt-get install -y git
wget https://go.dev/dl/go1.24.0.linux-amd64.tar.gz
sudo tar -C /usr/local -xzf go1.24.0.linux-amd64.tar.gz
export PATH=$PATH:/usr/local/go/bin

# Clone repo and build
git clone https://github.com/your-org/bazel-firecracker.git
cd bazel-firecracker
go build -o snapshot-builder ./cmd/snapshot-builder

# Copy rootfs and kernel (upload from local or download)
sudo mkdir -p /opt/firecracker
# SCP from local: gcloud compute scp images/microvm/output/* snapshot-builder-vm:/opt/firecracker/
# Or download from wherever you've staged them

# Install firecracker
FCVER=1.10.1
curl -L "https://github.com/firecracker-microvm/firecracker/releases/download/v${FCVER}/firecracker-v${FCVER}-x86_64.tgz" | tar xz
sudo mv release-v${FCVER}-x86_64/firecracker-v${FCVER}-x86_64 /usr/local/bin/firecracker
sudo chmod +x /usr/local/bin/firecracker

# Set up networking for the warmup VM
sudo ip link add fcbr0 type bridge
sudo ip addr add 172.16.0.1/24 dev fcbr0
sudo ip link set fcbr0 up
echo 1 | sudo tee /proc/sys/net/ipv4/ip_forward
sudo iptables -t nat -A POSTROUTING -s 172.16.0.0/24 -j MASQUERADE

# Run snapshot builder
sudo ./snapshot-builder \
--repo-url=https://github.com/your-org/your-repo \
--repo-branch=main \
--kernel-path=/opt/firecracker/kernel.bin \
--rootfs-path=/opt/firecracker/rootfs.img \
--gcs-bucket=your-bucket-name \
--github-app-id=YOUR_GITHUB_APP_ID \
--github-app-secret=projects/your-project-id/secrets/github-app-key/versions/latest

# Verify upload
gsutil ls gs://your-bucket-name/current/

Exit the VM:
exit

# Clean up builder VM
gcloud compute instances delete snapshot-builder-vm \
--zone=us-central1-a --project=your-project-id --quiet

Phase 11: (Optional) Build data snapshot for fast host boot

Skip this for initial deployment. Without it, hosts download from GCS on first boot (~5-15 min). You can add it later once the basic flow works.

Phase 12: Start hosts via MIG rolling update

make mig-rolling-update PROJECT_ID=your-project-id

# Watch hosts come up
watch -n5 gcloud compute instance-groups managed list-instances \
fc-runner-dev-hosts --region=us-central1 --project=your-project-id

Phase 13: Verify hosts

# Wait for instances to show RUNNING, then SSH to one
INSTANCE=$(gcloud compute instance-groups managed list-instances \
fc-runner-dev-hosts --region=us-central1 --project=your-project-id \
--format="value(instance)" --limit=1)

gcloud compute ssh "$INSTANCE" --zone=us-central1-a --project=your-project-id

# On the host:
sudo journalctl -u firecracker-manager --no-pager -n 50
curl http://localhost:8080/health
# Check bridge and iptables
ip addr show fcbr0
sudo iptables -t nat -L -n

Phase 14: Configure GitHub webhook

# Get control plane external IP
CP_IP=$(kubectl get svc control-plane -o jsonpath='{.status.loadBalancer.ingress[0].ip}')
echo "Webhook URL: http://${CP_IP}:8080/webhook/github"

In GitHub (your-org/your-repo):
1. Settings -> Webhooks -> Add webhook
2. Payload URL: http://<CP_IP>:8080/webhook/github
3. Content type: application/json
4. Events: select Workflow jobs

Phase 15: End-to-end test

Create a test workflow in your-org/your-repo:

# .github/workflows/firecracker-test.yaml
name: Firecracker Test
on: workflow_dispatch
jobs:
test:
runs-on: [self-hosted, firecracker]
steps:
- run: echo "Hello from Firecracker microVM"
- run: uname -a
- run: ip addr show
- uses: actions/checkout@v4
- run: bazel version

Trigger it manually from GitHub Actions, then watch the logs:

# Control plane (webhook -> allocate)
kubectl logs -l app=control-plane -f

# Host agent (VM restore -> thaw-agent)
gcloud compute ssh "$INSTANCE" --zone=us-central1-a -- \
sudo journalctl -u firecracker-manager -f

# Check runner appeared in GitHub
# Settings -> Actions -> Runners

Troubleshooting checklist

If things go wrong, check in this order:
┌─────────────────────────┬───────────────────────────────────────────────────────────────────┐
│         Symptom         │                               Check                               │
├─────────────────────────┼───────────────────────────────────────────────────────────────────┤
│ Host won't join MIG     │ gcloud compute instances get-serial-port-output INSTANCE          │
├─────────────────────────┼───────────────────────────────────────────────────────────────────┤
│ Manager won't start     │ journalctl -u firecracker-manager -- look for snapshot/KVM errors │
├─────────────────────────┼───────────────────────────────────────────────────────────────────┤
│ No snapshots available  │ gsutil ls gs://your-bucket-name/current/                          │
├─────────────────────────┼───────────────────────────────────────────────────────────────────┤
│ VM boots but no network │ SSH to host, check ip addr show fcbr0 and iptables -t nat -L      │
├─────────────────────────┼───────────────────────────────────────────────────────────────────┤
│ Thaw-agent won't start  │ From host: curl http://172.16.0.2:8080/health                     │
├─────────────────────────┼───────────────────────────────────────────────────────────────────┤
│ Runner won't register   │ From host: curl http://172.16.0.2:8080/mmds-diag -- check token   │
├─────────────────────────┼───────────────────────────────────────────────────────────────────┤
│ Webhook not received    │ kubectl logs -l app=control-plane -- check HTTP handler           │
├─────────────────────────┼───────────────────────────────────────────────────────────────────┤
│ DNS fails in microVM    │ From host: curl http://172.16.0.2:8080/connectivity               │
└─────────────────────────┴───────────────────────────────────────────────────────────────────┘
