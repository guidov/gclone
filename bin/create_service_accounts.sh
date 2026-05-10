#!/usr/bin/env bash
set -euo pipefail

# Creates N service accounts in a GCP project, downloads their JSON keys,
# and places them in the gclone accounts directory as 1.json, 2.json, etc.

ACCOUNTS_DIR="$(dirname "$0")/../accounts"
NUM_ACCOUNTS="${1:-5}"
PROJECT="${2:-}"

usage() {
    echo "Usage: $0 [num_accounts] [project_id]"
    echo "  num_accounts  Number of service accounts to create (default: 5)"
    echo "  project_id    GCP project ID (default: current gcloud project)"
    exit 1
}

# Resolve project
if [[ -z "$PROJECT" ]]; then
    PROJECT=$(gcloud config get-value project 2>/dev/null)
    if [[ -z "$PROJECT" ]]; then
        echo "Error: no GCP project set. Run 'gcloud config set project PROJECT_ID' or pass it as the second argument."
        exit 1
    fi
fi

echo "Project : $PROJECT"
echo "Accounts: $NUM_ACCOUNTS"
echo "Output  : $ACCOUNTS_DIR"
echo ""

mkdir -p "$ACCOUNTS_DIR"

for i in $(seq 1 "$NUM_ACCOUNTS"); do
    SA_NAME="gclone-sa${i}"
    SA_EMAIL="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
    KEY_FILE="${ACCOUNTS_DIR}/${i}.json"

    if [[ -f "$KEY_FILE" ]]; then
        echo "[$i/$NUM_ACCOUNTS] $KEY_FILE already exists, skipping."
        continue
    fi

    echo "[$i/$NUM_ACCOUNTS] Creating service account: $SA_NAME ..."

    # Create the service account (ignore error if it already exists)
    if ! gcloud iam service-accounts describe "$SA_EMAIL" --project="$PROJECT" &>/dev/null; then
        gcloud iam service-accounts create "$SA_NAME" \
            --project="$PROJECT" \
            --display-name="gclone Service Account $i" \
            --quiet
        sleep 5  # wait for SA to propagate before creating key
    else
        echo "             (account already exists, just downloading key)"
    fi

    echo "[$i/$NUM_ACCOUNTS] Downloading key to $KEY_FILE ..."
    CLOUDSDK_CORE_PROJECT="$PROJECT" gcloud iam service-accounts keys create "$KEY_FILE" \
        --iam-account="$SA_EMAIL" \
        --quiet

    echo "[$i/$NUM_ACCOUNTS] Done: $SA_EMAIL"
done

echo ""
echo "All service accounts ready. Their emails (share these with your Google Drive):"
echo ""
for f in $(ls "$ACCOUNTS_DIR"/*.json | sort -V); do
    python3 -c "import json; d=json.load(open('$f')); print(d['client_email'])" 2>/dev/null || echo "WARNING: $f is empty or invalid — re-run the script to regenerate it"
done
echo ""
echo "Share your Google Drive root folder or Shared Drive with each email above (Editor access)."
