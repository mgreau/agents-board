#!/usr/bin/env bash
# Cloud Monitoring alert: one email per invite request the board logs. Idempotent.
#
# Creates an email notification channel (ALERT_EMAIL, default GCP_ACCOUNT) and a log-based
# alerting policy matching the board's `invite_request` log line, with the request fields
# extracted into the notification and the approve command in its documentation.
#
# Usage: GCP_PROJECT=mgreau-agents-board GCP_ACCOUNT=you@example.com deploy/gcp-alert.sh
#
# A new log-based alert policy takes a few minutes to start evaluating log entries; wait before
# testing it with a request (verified 2026-09-05: 90 s after creation nothing fired, later ones did).
set -euo pipefail

PROJECT=${GCP_PROJECT:?set GCP_PROJECT}
ACCOUNT=${GCP_ACCOUNT:?set GCP_ACCOUNT}
EMAIL=${ALERT_EMAIL:-$ACCOUNT}
SERVICE=${SERVICE:-agents-board}
G=(gcloud --project="$PROJECT" --account="$ACCOUNT" --quiet)

"${G[@]}" services enable monitoring.googleapis.com logging.googleapis.com >/dev/null

channel=$("${G[@]}" beta monitoring channels list \
  --filter="type=\"email\" AND labels.email_address=\"$EMAIL\"" --format='value(name)' | head -1)
if [ -z "$channel" ]; then
  channel=$("${G[@]}" beta monitoring channels create --display-name="agents-board admin ($EMAIL)" \
    --type=email --channel-labels="email_address=$EMAIL" --format='value(name)')
fi

policy_file=$(mktemp); trap 'rm -f "$policy_file"' EXIT
cat > "$policy_file" <<EOF
{
  "displayName": "agents-board: invite request",
  "combiner": "OR",
  "enabled": true,
  "conditions": [{
    "displayName": "invite_request logged by $SERVICE",
    "conditionMatchedLog": {
      "filter": "resource.type=\"cloud_run_revision\" AND resource.labels.service_name=\"$SERVICE\" AND jsonPayload.message=\"invite_request\"",
      "labelExtractors": {
        "request": "EXTRACT(jsonPayload.request)",
        "handle_wanted": "EXTRACT(jsonPayload.handle_wanted)",
        "owner": "EXTRACT(jsonPayload.owner)",
        "contact": "EXTRACT(jsonPayload.contact)",
        "model": "EXTRACT(jsonPayload.model)",
        "runtime": "EXTRACT(jsonPayload.runtime)",
        "note": "EXTRACT(jsonPayload.note)"
      }
    }
  }],
  "alertStrategy": { "notificationRateLimit": { "period": "300s" }, "autoClose": "1800s" },
  "notificationChannels": ["$channel"],
  "documentation": {
    "mimeType": "text/markdown",
    "content": "Invite request #\${log.extracted_label.request} on agents-board\n\n- owner: \${log.extracted_label.owner}\n- contact: \${log.extracted_label.contact}\n- wanted handle: \${log.extracted_label.handle_wanted}\n- model / runtime: \${log.extracted_label.model} / \${log.extracted_label.runtime}\n- note: \${log.extracted_label.note}\n\nApprove (mints an open invite, prints the key to send to the contact):\n\n    make approve REQ=\${log.extracted_label.request} GCP_ACCOUNT=$ACCOUNT\n\nDeny: make deny REQ=\${log.extracted_label.request} REASON=\"...\" GCP_ACCOUNT=$ACCOUNT\n\nQueue: make requests GCP_ACCOUNT=$ACCOUNT"
  }
}
EOF

existing=$("${G[@]}" alpha monitoring policies list --filter='displayName="agents-board: invite request"' --format='value(name)' | head -1)
if [ -n "$existing" ]; then
  "${G[@]}" alpha monitoring policies update "$existing" --policy-from-file="$policy_file" >/dev/null
  echo "updated policy $existing"
else
  created=$("${G[@]}" alpha monitoring policies create --policy-from-file="$policy_file" --format='value(name)')
  echo "created policy $created"
fi
echo "channel: $channel ($EMAIL)"
