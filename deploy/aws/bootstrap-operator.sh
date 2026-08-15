#!/usr/bin/env bash
# bootstrap-operator.sh — one-time, HUMAN-run setup of the scoped AWS
# operator identity for culture-nodes automation.
#
#   ./deploy/aws/bootstrap-operator.sh [profile-name]   # default: culture-nodes
#   ./deploy/aws/bootstrap-operator.sh update-policy     # re-apply dev-operator-policy.json
#   ./deploy/aws/bootstrap-operator.sh enable-region <region>   # opt in to a region
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

MODE="${1:-bootstrap}"
case "$MODE" in
  update-policy) PROFILE="${2:-culture-nodes}" ;;
  enable-region) PROFILE="${3:-culture-nodes}" ;;
  *)             PROFILE="${1:-culture-nodes}" ;;
esac
USER_NAME="culture-nodes-dev"
POLICY_NAME="culture-nodes-dev-operator"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLICY_FILE="$SCRIPT_DIR/dev-operator-policy.json"
REGION="${AWS_REGION:-$(aws configure get region 2>/dev/null || echo us-east-1)}"

echo "==> bootstrap identity: $(aws sts get-caller-identity --query Arn --output text)"
ACCOUNT=$(aws sts get-caller-identity --query Account --output text)

if [ "$MODE" = "enable-region" ]; then
  # Opt in to a region. This is an ACCOUNT-LEVEL setting, which is why it
  # lives with the admin identity and not in the scoped operator policy:
  # a credential that can turn regions on for the whole account is not a
  # scoped credential. The operator policy grants only the READ
  # (account:GetRegionOptStatus) so preflight.py can report the status.
  #
  # Until a region is enabled, EVERY API call to it fails with
  # InvalidClientTokenId — a signature that reads like a broken credential
  # and is not one. That confusion is the reason this mode exists.
  TARGET_REGION="${2:?usage: bootstrap-operator.sh enable-region <region> [profile]}"
  STATUS=$(aws account get-region-opt-status --region-name "$TARGET_REGION" \
    --query RegionOptStatus --output text 2>/dev/null || echo UNKNOWN)
  echo "==> $TARGET_REGION current status: $STATUS"
  case "$STATUS" in
    ENABLED|ENABLED_BY_DEFAULT)
      echo "==> already enabled — nothing to do"; exit 0 ;;
    ENABLING)
      echo "==> enable already in progress; it takes a few minutes. Re-run"
      echo "    ./deploy/aws/preflight.py --region $TARGET_REGION to watch for it."
      exit 0 ;;
  esac
  aws account enable-region --region-name "$TARGET_REGION"
  echo "==> enable requested for $TARGET_REGION"
  echo "==> this takes a few minutes to propagate. Watch it with:"
  echo "      ./deploy/aws/preflight.py --region $TARGET_REGION"
  exit 0
fi

if [ "$MODE" = "update-policy" ]; then
  # Re-apply the committed JSON as the default policy version. IAM caps a
  # policy at 5 versions, so the oldest non-default is pruned first when
  # the cap is hit — versions are an edit trail here, not a rollback store
  # (the JSON in git is the rollback store).
  POLICY_ARN="arn:aws:iam::${ACCOUNT}:policy/${POLICY_NAME}"
  COUNT=$(aws iam list-policy-versions --policy-arn "$POLICY_ARN"     --query 'length(Versions)' --output text)
  if [ "$COUNT" -ge 5 ]; then
    OLDEST=$(aws iam list-policy-versions --policy-arn "$POLICY_ARN"       --query 'Versions[?IsDefaultVersion==`false`] | sort_by(@, &CreateDate)[0].VersionId' --output text)
    aws iam delete-policy-version --policy-arn "$POLICY_ARN" --version-id "$OLDEST"
    echo "==> pruned oldest non-default version $OLDEST (5-version cap)"
  fi
  NEWV=$(aws iam create-policy-version --policy-arn "$POLICY_ARN"     --policy-document "file://$POLICY_FILE" --set-as-default     --query 'PolicyVersion.VersionId' --output text)
  echo "==> $POLICY_NAME now at version $NEWV (default), from $POLICY_FILE"
  exit 0
fi

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
