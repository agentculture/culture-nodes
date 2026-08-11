#!/usr/bin/env bash
# bootstrap-operator.sh — one-time, HUMAN-run setup of the scoped AWS
# operator identity for culture-nodes automation.
#
#   ./deploy/aws/bootstrap-operator.sh [profile-name]   # default: culture-nodes
#
# Run this yourself with admin (or root, first-time-only) credentials
# active. It creates the culture-nodes-dev IAM user, attaches the
# dev-operator policy (everything fenced to culture-nodes-* resources —
# see dev-operator-policy.json), mints ONE access key, and lands it
# directly in a named CLI profile via `aws configure set` — the secret is
# captured into a shell variable and written by the AWS CLI itself, so it
# is never echoed to the terminal or a log.
#
# Agents (Claude, colleague, codex sessions) then operate on the scoped
# profile and NEVER on the bootstrap credential; per standing policy they
# do not run this script or handle key material at all. After this
# succeeds, stop using root for CLI work.
set -euo pipefail

PROFILE="${1:-culture-nodes}"
USER_NAME="culture-nodes-dev"
POLICY_NAME="culture-nodes-dev-operator"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY_FILE="$SCRIPT_DIR/dev-operator-policy.json"
REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo us-east-1)}"

echo "==> bootstrap identity: $(aws sts get-caller-identity --query Arn --output text)"
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)

if aws iam get-user --user-name "$USER_NAME" >/dev/null 2>&1; then
  echo "==> user $USER_NAME already exists (kept)"
else
  aws iam create-user --user-name "$USER_NAME" >/dev/null
  echo "==> created user $USER_NAME"
fi

POLICY_ARN="arn:aws:iam::${ACCOUNT}:policy/${POLICY_NAME}"
if aws iam get-policy --policy-arn "$POLICY_ARN" >/dev/null 2>&1; then
  echo "==> policy $POLICY_NAME already exists (kept — update it via a new version if the JSON changed)"
else
  aws iam create-policy --policy-name "$POLICY_NAME" \
    --policy-document "file://$POLICY_FILE" >/dev/null
  echo "==> created policy $POLICY_NAME from $POLICY_FILE"
fi

aws iam attach-user-policy --user-name "$USER_NAME" --policy-arn "$POLICY_ARN"
echo "==> policy attached"

KEY_COUNT=$(aws iam list-access-keys --user-name "$USER_NAME" \
  --query 'length(AccessKeyMetadata)' --output text)
if [ "$KEY_COUNT" != "0" ] && [ "${FORCE:-0}" != "1" ]; then
  echo "==> $USER_NAME already has $KEY_COUNT access key(s); refusing to mint another (FORCE=1 to add one — remember the 2-key limit)"
else
  KEY_JSON=$(aws iam create-access-key --user-name "$USER_NAME" \
    --query 'AccessKey.[AccessKeyId,SecretAccessKey]' --output text)
  AKID=$(cut -f1 <<<"$KEY_JSON"); SECRET=$(cut -f2 <<<"$KEY_JSON")
  aws configure set aws_access_key_id "$AKID" --profile "$PROFILE"
  aws configure set aws_secret_access_key "$SECRET" --profile "$PROFILE"
  aws configure set region "$REGION" --profile "$PROFILE"
  aws configure set output json --profile "$PROFILE"
  unset KEY_JSON AKID SECRET
  echo "==> access key minted and written straight into profile '$PROFILE' (never echoed)"
fi

echo "==> verify: aws sts get-caller-identity --profile $PROFILE"
aws sts get-caller-identity --profile "$PROFILE" --query Arn --output text
echo "==> done. Automation should now use --profile $PROFILE (or AWS_PROFILE=$PROFILE); stop using the bootstrap credential for CLI work."
