#!/usr/bin/env bash
# Kubernetes probes for usersync-smb, from the state entrypoint.sh leaves in
# /run/usersync. A plain tcpSocket probe on 445 cannot tell "this pod has not
# taken over yet" from "this pod's smbd died", and under the hand-off
# (SMB_HANDOFF=1) the first is the normal state of a new pod for a few seconds —
# and it is exactly when the readiness probe must PASS, because the old pod is
# only stopped once the new one is Ready.
#
#   probe.sh ready  prepared (accounts, shares, winbindd). The old pod may go.
#   probe.sh live   until this pod's smbd took 445, alive; after, 445 must be open.
#
# Use `ready` for startupProbe and readinessProbe, `live` for livenessProbe.
set -u
STATE_DIR=/run/usersync

listening_445() {
	local files=() f
	for f in /proc/net/tcp /proc/net/tcp6; do [[ -r $f ]] && files+=("$f"); done
	awk 'FNR > 1 && $4 == "0A" && $2 ~ /:01BD$/ { found = 1 } END { exit !found }' "${files[@]}"
}

case ${1:-} in
ready)
	[[ -e $STATE_DIR/prepared ]] || exit 1
	# Once serving, readiness is "445 is open", as it was before the hand-off.
	[[ -e $STATE_DIR/serving ]] && { listening_445 || exit 1; }
	exit 0
	;;
live)
	[[ -e $STATE_DIR/serving ]] || exit 0
	listening_445
	;;
*)
	echo "usage: probe.sh ready|live" >&2
	exit 2
	;;
esac
