#!/bin/bash
# Sets up kubectl port-forward to Capsule control plane + SSH tunnel to the active host.
# Run this once before boot_claw.py. Re-run if tunnels drop.
set -euo pipefail

PROJECT="${CAPSULE_PROJECT:-salessavvy-test}"
CLUSTER="capsule-test-control-plane"
CLUSTER_ZONE="us-central1-c"
CP_PORT="${CAPSULE_CP_PORT:-8081}"
HOST_PORT="${CAPSULE_HOST_PORT:-9090}"

echo "=== Getting GKE credentials ==="
gcloud container clusters get-credentials "$CLUSTER" \
 --zone "$CLUSTER_ZONE" --project "$PROJECT" --dns-endpoint 2>&1

echo ""
echo "=== Starting control plane port-forward (localhost:$CP_PORT) ==="
pkill -f "port-forward.*capsule" 2>/dev/null || true
sleep 1
kubectl port-forward deployment/control-plane -n capsule "$CP_PORT:8080" &
sleep 5

echo ""
echo "=== Checking control plane ==="
for i in $(seq 1 5); do
 if curl -s "http://localhost:$CP_PORT/health" | grep -q OK; then
  echo "  Control plane OK"
  break
 fi
 echo "  Waiting for port-forward... ($i)"
 sleep 2
done

echo ""
echo "=== Finding active Capsule host ==="
HOST_INFO=$(curl -s "http://localhost:$CP_PORT/api/v1/hosts" | \
 python3 -c "
import sys, json
hosts = json.load(sys.stdin).get('hosts', [])
for h in hosts:
  if h['status'] == 'ready':
    print(f\"{h['instance_name']} {h['zone']}\")
    break
else:
  print('NONE')
")

if [ "$HOST_INFO" = "NONE" ]; then
 echo "  ERROR: No ready hosts found"
 exit 1
fi

HOST_NAME=$(echo "$HOST_INFO" | awk '{print $1}')
HOST_ZONE=$(echo "$HOST_INFO" | awk '{print $2}')
echo "  Host: $HOST_NAME ($HOST_ZONE)"

echo ""
echo "=== Setting up SSH tunnel to host (localhost:$HOST_PORT) ==="
pkill -f "ssh.*capsule-test-host" 2>/dev/null || true
sleep 1
gcloud compute ssh "$HOST_NAME" \
 --zone "$HOST_ZONE" --project "$PROJECT" \
 --tunnel-through-iap \
 --ssh-flag="-N" --ssh-flag="-L" --ssh-flag="$HOST_PORT:localhost:8080" &
sleep 15

for i in $(seq 1 5); do
 if curl -s "http://localhost:$HOST_PORT/health" | grep -q OK; then
  echo "  Host tunnel OK"
  break
 fi
 echo "  Waiting for SSH tunnel... ($i)"
 sleep 3
done

echo ""
echo "=== Fixing host iptables (eth0 -> ens4 bug) ==="
#gcloud compute ssh "$HOST_NAME" \
# --zone "$HOST_ZONE" --project "$PROJECT" \
# --tunnel-through-iap \
# --command "
#sudo iptables -D FORWARD -s 10.200.0.0/16 -o eth0 -j ACCEPT 2>/dev/null
#sudo iptables -A FORWARD -s 10.200.0.0/16 -o ens4 -j ACCEPT
#sudo iptables -D FORWARD -d 10.200.0.0/16 -i eth0 -m state --state RELATED,ESTABLISHED -j ACCEPT 2>/dev/null
#sudo iptables -A FORWARD -d 10.200.0.0/16 -i ens4 -m state --state RELATED,ESTABLISHED -j ACCEPT
#sudo iptables -t nat -D POSTROUTING -s 10.200.0.0/16 -o eth0 -j MASQUERADE 2>/dev/null
#sudo iptables -t nat -A POSTROUTING -s 10.200.0.0/16 -o ens4 -j MASQUERADE
#echo 'iptables fixed'
#" 2>&1 | tail -1

echo ""
echo "============================================================"
echo " Tunnels ready!"
echo " Control plane: http://localhost:$CP_PORT"
echo " Host proxy:  http://localhost:$HOST_PORT"
echo ""
echo " Now run:"
echo "  export CAPSULE_BASE_URL=http://localhost:$CP_PORT"
echo "  export CAPSULE_HOST_OVERRIDE=localhost:$HOST_PORT"
echo "  python boot_claw.py"
echo "============================================================"
