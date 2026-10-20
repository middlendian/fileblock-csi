#!/usr/bin/env bash
# Manual reproduction of an NFS server answering ESTALE for a short window,
# the condition failed-volume remediation exists for. Not run in CI: it
# depends on timing. Run against a `make e2e-nfs` cluster (KEEP=1) with a pod
# writing continuously to a fileblock volume, for example:
#
#   kubectl -n ns exec deploy/writer -- sh -c 'while :; do date >> /data/log; sync; sleep 1; done'
#
#   hack/estale-repro.sh /path/to/export 30
#
# Unexports the directory for N seconds (default 30) and exports it again.
# In-flight I/O gets ESTALE and ext4 aborts during the outage. The monitor
# should flag the volume and delete the writer pod within one check
# interval of that abort, without waiting for the export to return.
set -euo pipefail
EXPORT="${1:?usage: $0 EXPORT_DIR [SECONDS]}"
SECS="${2:-30}"
SUDO="${SUDO:-sudo}"

# The options hack/e2e.sh's prepare_backing_nfs exports with.
reexport() {
  $SUDO exportfs -o rw,sync,no_subtree_check,no_root_squash,insecure "*:${EXPORT}"
  echo "re-exported ${EXPORT}"
}

$SUDO exportfs -u "*:${EXPORT}"
# Interrupting the sleep must not leave the export down.
trap reexport EXIT
echo "unexported ${EXPORT}; sleeping ${SECS}s"
sleep "${SECS}"
