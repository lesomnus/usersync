#!/usr/bin/env bash
# usersync-smb entrypoint: bring up the SMB file server from the roster.
#
# Accounts, tdbsam, the group/home folders, and smb.conf's share block are all
# rebuilt from roster.yaml on every start — derived state pinned by the roster's
# numbers (nas-design ADR-9). The container then runs `usersync watch
# --reload-smb`, which re-applies the roster and reloads smbd on every change, so
# an account add / rename / team change lands WITHOUT a restart.
#
# This is the serving half of the darak split: smbd + winbindd + usersync, and
# NO darak binary — darak does nothing special to SMB, so the web tier is a
# separate image and this one owns the shared state (tdbsam, folders, shares).
set -euo pipefail

CONFIG_DIR=${USERSYNC_CONFIG_DIR:-/etc/usersync}
SMB_WORKGROUP=${SMB_WORKGROUP:-WORKGROUP}
SMB_SERVER_STRING=${SMB_SERVER_STRING:-darak}
# Operator drop-in for [global] settings the image does not bake in — e.g.
# `lock directory` (to point smbstatus/profiling at a mounted path), `max log
# size`, `smbd profiling level`. Samba's own `include` reads it inline at the END
# of [global], so its parameters OVERRIDE the seeded defaults above them, and a
# MISSING file is silently ignored — so this is always seeded and only takes
# effect when the operator mounts a file here. Default sits in the config mount
# the operator already provides, so no extra volume is needed.
SMB_GLOBAL_INCLUDE=${SMB_GLOBAL_INCLUDE:-$CONFIG_DIR/smb.global.conf}
# Optional persistent directory for the account cache (see "account cache"
# below). Unset = no cache: every start rebuilds the accounts from scratch.
ACCOUNTS_DIR=${USERSYNC_ACCOUNTS_DIR:-}

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() {
	printf '\033[1;31mfatal:\033[0m %s\n' "$*" >&2
	exit 1
}

[[ -f "$CONFIG_DIR/roster.yaml" ]] || die "no roster at $CONFIG_DIR/roster.yaml — mount it"
[[ -f "$CONFIG_DIR/usersync.yaml" ]] || die "no config at $CONFIG_DIR/usersync.yaml — mount it"

# usersync resolves usersync.yaml and roster.yaml from the working directory, and
# `watch` polls roster.yaml relative to it — so run everything from the mount.
cd "$CONFIG_DIR"

# --- layout -----------------------------------------------------------------
#
# usersync creates each home/group folder with MkdirAll, which gives every
# intermediate directory the LEAF's mode — so the shared roots must exist with
# the right perms FIRST, or an absent parent would be created 0700 and nobody
# could traverse into their own home. Read the roots from the config so this
# stays in step with paths.home / paths.groups instead of hardcoding them.
HOME_BASE=$(usersync config | sed -n 's/^[[:space:]]*home:[[:space:]]*//p' | tr -d '"' | head -1)
GROUPS_BASE=$(usersync config | sed -n 's/^[[:space:]]*groups:[[:space:]]*//p' | tr -d '"' | head -1)
[[ -n $HOME_BASE && -n $GROUPS_BASE ]] || die "could not read paths.home/paths.groups from usersync config"

install -d -m 0755 "$(dirname "$HOME_BASE")" "$(dirname "$GROUPS_BASE")"
# homes is 0711 (traverse-only; the r bit would only list everyone's username),
# teams is 0755 (a team name is not personal); the folders under teams are 2770,
# which usersync sets.
install -d -m 0711 "$HOME_BASE"
install -d -m 0755 "$GROUPS_BASE"

# --- samba config -----------------------------------------------------------
log "samba (config)"
install -d -m 0755 /var/lib/samba/private /var/log/samba /run/samba

if [[ ! -f /etc/samba/smb.conf ]]; then
	# The share block usersync manages is spliced into this file; the global
	# section is the operator's to own, so seed it once and never again. full_audit
	# lives in [global] so it covers every share (including usersync's generated
	# ones) and so darak — in the web pod, reading this same audit log off the
	# shared volume — still sees SMB activity after the split. See
	# scripts/verify-samba-modes.sh for how the modes were checked against a real
	# smbd. An operator who mounts their own smb.conf still wins.
	cat >/etc/samba/smb.conf <<-EOF
		[global]
		   workgroup = ${SMB_WORKGROUP}
		   server string = ${SMB_SERVER_STRING}
		   security = user
		   passdb backend = tdbsam
		   map to guest = never
		   log level = 1
		   log file = /var/log/samba/audit.log
		   max log size = 5000
		   disable netbios = yes
		   vfs objects = full_audit
		   full_audit:prefix = %u|%I|%S
		   full_audit:success = create_file mkdirat unlinkat renameat
		   full_audit:failure = none
		   full_audit:syslog = no
		   include = ${SMB_GLOBAL_INCLUDE}
	EOF
fi

# --- account cache ------------------------------------------------------------
#
# Restarts used to recreate every account from scratch (16 s of a 17 s restart
# with 82 users). ACCOUNTS_DIR keeps them; deploy/accounts-cache.sh has the why.
# shellcheck source=deploy/accounts-cache.sh
source /usr/local/lib/usersync/accounts-cache.sh
accounts_cache_init

# --- accounts + shares ------------------------------------------------------
#
# Static check first, with no system access: a typo in the roster should be a
# refusal to start, not a half-applied set of accounts.
usersync validate

# `mode: audit` means a directory service owns the accounts now, and usersync
# refuses to create any — read the mode rather than assuming this pod still makes
# accounts, so a cutover does not fail the boot.
mode=$(usersync config 2>/dev/null | sed -n 's/^mode:[[:space:]]*//p' | tr -d '"' | head -1)
case "${mode:-manage}" in
audit)
	log "mode: audit — a directory owns the accounts; verifying instead of applying"
	usersync audit || echo "WARNING: the directory and the roster disagree; see above" >&2
	;;
*)
	# Show what will change before it changes. On a healthy restart this is empty.
	usersync plan || die "usersync refused the roster; nothing has been changed"
	usersync apply
	save_accounts
	;;
esac

# Render the share block into smb.conf before smbd starts — no reload yet, smbd
# is not up. `usersync watch --reload-smb` below re-renders and reloads on every
# later roster change.
usersync shares --write

# A wrong operation name in full_audit:success does NOT degrade to "no auditing"
# — it makes every share REFUSE TO CONNECT, and testparm does not catch it. So
# ask the module itself, before smbd is serving anything. The op names live
# NUL-terminated in the .so, so tr does what strings would without binutils.
check_audit_ops() {
	local so missing=()
	so=$(ls /usr/lib/*/samba/vfs/full_audit.so 2>/dev/null | head -1)
	if [[ -z $so ]]; then
		missing=(full_audit.so)
	else
		for op in create_file mkdirat unlinkat renameat; do
			tr '\0' '\n' <"$so" | grep -qx "$op" || missing+=("$op")
		done
	fi
	if ((${#missing[@]})); then
		echo "WARNING: this Samba does not know: ${missing[*]}" >&2
		echo "  Removing the audit block rather than serving shares that refuse to connect." >&2
		sed -i '/vfs objects = full_audit/d; /full_audit:/d' /etc/samba/smb.conf
	fi
}
check_audit_ops

# --- serve ------------------------------------------------------------------
#
# Two ways to come up, chosen by SMB_HANDOFF:
#
#   unset  start smbd now. Under Recreate the old pod is already gone, so the
#          ports are free; 445 is closed from the old pod's exit until here —
#          ~1 s with the account cache.
#   1      the hand-off. The new pod starts NEXT TO the old one (RollingUpdate,
#          maxSurge 1) and does everything that takes time — image pull, account
#          apply, shares, winbindd — while the old smbd still serves. Then it says
#          "prepared" (the readiness probe), Kubernetes stops the old pod, and
#          the moment the old smbd lets go of 139/445 this one starts smbd. 445 is
#          closed only for smbd's own start.
#
# Either way, open files are not carried over: Samba has no persistent handles,
# so a client reconnects and reopens. The hand-off shortens the gap, not that.
#
# The probes read the state this leaves in STATE_DIR (see probe.sh):
#   prepared  everything but smbd is ready — the old pod may be stopped now
#   serving   this pod's smbd took 445; from here 445 must stay open
STATE_DIR=/run/usersync
install -d -m 0755 "$STATE_DIR"
rm -f "$STATE_DIR/prepared" "$STATE_DIR/serving"

# Is anything listening on the given ports (hex)? Read from /proc/net: this pod is
# on the host network, so the old pod's smbd shows there too. No ss in the image.
listening() {
	local files=() f
	for f in /proc/net/tcp /proc/net/tcp6; do [[ -r $f ]] && files+=("$f"); done
	awk -v ports="$*" '
		BEGIN { n = split(ports, p, " "); for (i = 1; i <= n; i++) want[":" p[i]] = 1 }
		FNR > 1 && $4 == "0A" { a = $2; sub(/^[^:]*/, "", a); if (a in want) found = 1 }
		END { exit !found }' "${files[@]}"
}
SMB_PORTS="01BD 008B" # 445 and 139 — smbd binds both (`smb ports` default)

# ntlm_auth is a winbind client even on a standalone server, and darak's web
# logins go through it — so wait for winbindd here, where a not-ready is one
# clear line, instead of at the first login as a helper error. winbindd's socket
# is in this pod's /run/samba, so it runs next to the old pod's without a clash.
log "winbindd"
winbindd -D
for _ in $(seq 100); do
	wbinfo -p >/dev/null 2>&1 && break
	sleep 0.2
done
wbinfo -p >/dev/null 2>&1 || die "winbindd did not become ready; web logins would all fail"

touch "$STATE_DIR/prepared"
if [[ ${SMB_HANDOFF:-} == 1 ]] && listening $SMB_PORTS; then
	log "hand-off: prepared; waiting for the old smbd to release 139/445"
	waited=0
	while listening $SMB_PORTS; do
		sleep 0.05
		waited=$((waited + 1))
		# Nothing stops the old pod but Kubernetes, once this one is Ready. Say so
		# now and then rather than wait silently if that never comes.
		if ((waited % 600 == 0)); then
			log "hand-off: still waiting ($((waited / 20)) s) — is the old pod being stopped?"
		fi
	done
fi

log "smbd"
started=$(date +%s.%N)
smbd -D
for _ in $(seq 200); do
	listening 01BD && break
	sleep 0.05
done
listening 01BD || die "smbd did not open 445"
log "smbd: 445 open $(awk -v a="$started" -v b="$(date +%s.%N)" 'BEGIN { printf "%.2f", b - a }') s after start"
touch "$STATE_DIR/serving"

# In audit mode usersync must not apply, and `watch` refuses a non-manage mode
# anyway — so there is nothing for it to reconcile. Keep the server up without it.
if [[ ${mode:-manage} == audit ]]; then
	log "mode: audit — hot-reload off; serving"
	exec sleep infinity
fi

# The reconcile loop: re-apply the roster and reload smbd on every change. smbd
# and winbindd are daemonized alongside it, exactly as in the single-container
# image before the split — so they are orphans, and PID 1 (tini, see the
# Dockerfile) is what reaps them and their helpers.
accounts_cache_follow

log "usersync watch --reload-smb"
exec usersync watch --reload-smb
