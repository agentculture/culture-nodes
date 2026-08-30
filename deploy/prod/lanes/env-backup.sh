# Timestamped backups for the two runner grant files (task t5, issue #253).
#
# Why this exists. On 2026-08-29 install-secrets.sh's Jira lane did a
# whole-file `cat >` over ~/.culture-nodes/runner-secrets.env and reduced it to
# 36 bytes, destroying three hand-granted sweep credentials. The runner
# boundary then refused every sweep attempt for sixteen hours. The merge in
# that lane is the fix; this is the seatbelt behind it — whatever a lane
# believes it is doing to one of these files, the bytes it replaced are still
# on the host afterwards and the deploy log says how to put them back.
#
# It is deliberately NOT a general utility. Only the two grant files are
# backed up (runner.env, runner-secrets.env), because those are the two whose
# contents accrete from outside this repo: prod.env has its own key-by-key
# merge and its own removal path (remove-secret.sh), and a backup of a file
# nothing can truncate is a second copy of a credential for no benefit.
#
# Sourced by deploy.sh (for lanes/runner-env-write.sh) and by
# install-secrets.sh (for the Jira lane). Both source it near the top so the
# helper exists before any lane runs.

# backup_env_file <ssh-target> <file-basename>
#
# Copies ~/.culture-nodes/<file> aside as <file>.bak-<UTC timestamp> and
# prints the command that restores it. A host that does not have the file yet
# (a first deploy) is not an error: there is nothing to protect.
#
# The timestamp is generated ON THE HOST, so it names the host's own clock —
# the same clock the file's mtime is stamped from — rather than the operator
# laptop's, which may be in another timezone or simply wrong.
#
# Backups hold live credentials, so they are mode 600 and BOUNDED: the ten
# most recent are kept and older ones are removed. An unbounded archive of
# credential files under ~/.culture-nodes is a slow leak, not a safety net.
backup_env_file() { # target, file basename under ~/.culture-nodes
  local target=$1 file=$2 backup
  # shellcheck disable=SC2016 # the expansions are deliberately remote
  backup=$(ssh "$target" "ENV_BACKUP_FILE=$file; "'
set -e
f="$HOME/.culture-nodes/$ENV_BACKUP_FILE"
[ -f "$f" ] || exit 0
umask 077
b="$f.bak-$(date -u +%Y%m%dT%H%M%SZ)"
cp -p "$f" "$b"
chmod 600 "$b"
# Retention ranks backups by their own UTC stamp -- their NAME -- and not by
# mtime. `cp -p` above gives a backup the mtime of the file it copied, so mtime
# is when the GRANT FILE was last written, which is not the order the backups
# were taken in: on a host whose grant file was restored from an older copy
# (a cp -p, an rsync -a, a snapshot), the backup just taken has the oldest
# mtime of all and `ls -t` sorts it last, so the retention step deletes the
# very bytes this call was made to preserve. The stamp is generated in
# sortable UTC precisely so a lexical sort IS chronological order.
#
# Its stdout goes to stderr. Whatever this step has to say is a LOG line,
# while the caller captures the stdout of this remote command as the backup
# path -- one stray byte on the wrong stream and the deploy log advertises a
# restore command for a path that does not exist.
ls -1 "$f".bak-* 2>/dev/null | sort -r | tail -n +11 | while IFS= read -r old; do rm -f "$old"; done >&2
# LAST, and newline-terminated: the path is the return value of this remote
# command, printed only once nothing else is going to write to this stream.
printf "%s\n" "$b"')
  if [ -z "$backup" ]; then
    printf '==> no ~/.culture-nodes/%s on %s yet — nothing to back up\n' "$file" "$target"
    return 0
  fi
  printf '==> backed up %s on %s to %s\n' "$file" "$target" "$backup"
  printf "==> restore it with: ssh %s 'cp %s ~/.culture-nodes/%s'\n" "$target" "$backup" "$file"
}
