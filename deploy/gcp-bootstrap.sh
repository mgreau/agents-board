#!/usr/bin/env bash
# One-time Google Cloud bootstrap for agents-board. Idempotent: safe to re-run.
#
# Creates, in $GCP_PROJECT:
#   - Artifact Registry docker repo  $GCP_REGION-docker.pkg.dev/$GCP_PROJECT/agents-board
#   - runtime service account        agents-board@$GCP_PROJECT.iam.gserviceaccount.com
#   - versioned snapshot bucket      gs://$GCP_PROJECT-snapshots (private, uniform access)
#   - secrets                        board-admin-token, board-edge-key (random 64 hex chars)
# and grants the runtime SA object access on the bucket and accessor on both secrets.
#
# Usage: GCP_PROJECT=mgreau-agents-board GCP_ACCOUNT=you@gmail.com deploy/gcp-bootstrap.sh
set -euo pipefail

PROJECT=${GCP_PROJECT:?set GCP_PROJECT}
ACCOUNT=${GCP_ACCOUNT:?set GCP_ACCOUNT}
REGION=${GCP_REGION:-us-central1}
SA_NAME=agents-board
SA="$SA_NAME@$PROJECT.iam.gserviceaccount.com"
BUCKET=${BOARD_BUCKET:-$PROJECT-snapshots}
AR_REPO=agents-board
G=(gcloud --project="$PROJECT" --account="$ACCOUNT" --quiet)

"${G[@]}" services enable run.googleapis.com artifactregistry.googleapis.com \
  secretmanager.googleapis.com storage.googleapis.com iam.googleapis.com >/dev/null

"${G[@]}" artifacts repositories describe "$AR_REPO" --location="$REGION" >/dev/null 2>&1 ||
  "${G[@]}" artifacts repositories create "$AR_REPO" --repository-format=docker \
    --location="$REGION" --description="agents-board container images"

"${G[@]}" iam service-accounts describe "$SA" >/dev/null 2>&1 ||
  "${G[@]}" iam service-accounts create "$SA_NAME" --display-name="agents-board runtime"

if ! "${G[@]}" storage buckets describe "gs://$BUCKET" >/dev/null 2>&1; then
  "${G[@]}" storage buckets create "gs://$BUCKET" --location="$REGION" \
    --uniform-bucket-level-access --public-access-prevention
fi
"${G[@]}" storage buckets update "gs://$BUCKET" --versioning >/dev/null
"${G[@]}" storage buckets add-iam-policy-binding "gs://$BUCKET" \
  --member="serviceAccount:$SA" --role=roles/storage.objectUser >/dev/null

for s in board-admin-token board-edge-key; do
  if ! "${G[@]}" secrets describe "$s" >/dev/null 2>&1; then
    python3 -c 'import secrets,sys; sys.stdout.write(secrets.token_hex(32))' |
      "${G[@]}" secrets create "$s" --data-file=- --replication-policy=automatic >/dev/null
  fi
  "${G[@]}" secrets add-iam-policy-binding "$s" \
    --member="serviceAccount:$SA" --role=roles/secretmanager.secretAccessor >/dev/null
done

cat <<OUT
project:  $PROJECT
region:   $REGION
sa:       $SA
bucket:   gs://$BUCKET
registry: $REGION-docker.pkg.dev/$PROJECT/$AR_REPO
secrets:  board-admin-token, board-edge-key
OUT
