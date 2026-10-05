# shellcheck shell=bash
# The account cache: keep the accounts usersync made across container restarts.
# Sourced, not run. Shared by deploy/smb-server/entrypoint.sh and by any other
# image that runs `usersync apply` at start — darak's web tier builds usersync
# from this module and copies this file out of the same pinned version.
#
# Caller provides log() and ACCOUNTS_DIR (empty = no cache), then calls
# accounts_cache_init before `usersync apply`, save_accounts after it, and
# accounts_cache_follow before handing the process over to `usersync watch`.
#
# Why: the container's /etc/passwd, group, shadow and gshadow are image files, so
# every start used to find ALL roster accounts missing and create them one by
# one: groupadd + useradd + usermod -G + usermod -L + smbpasswd -e per user,
# ~0.2 s each. With 82 users that was 16 s of a 17 s restart, during which
# nothing listens on 445 (measured 2026-10-05; smbd itself starts in 0.3 s).
#
# So the entries usersync made are kept in ACCOUNTS_DIR (a persistent volume)
# and merged back into /etc before `apply`. Then a restart's apply finds the
# accounts present and only does what the roster changed.
#
# It is a CACHE, not a second source of truth: `apply` still reconciles
# against the roster, so a stale or partial copy only costs time — a missing
# account is created, a wrong membership is corrected, an account the roster
# dropped is disabled exactly as on a host whose /etc persists (which is what
# usersync was written for). Anything the CALLER adds outside the roster is
# cached too; the caller must re-derive it after accounts_cache_init.
#
# Why not mount the four files straight from the host: shadow-utils writes
# `<file>+` and rename()s it over the original, and a rename onto a bind-mounted
# file fails — `groupadd: failure while writing changes to /etc/group`. So every
# later account change would break. The cache is plain files in a directory.
#
# One directory per container role: two containers that keep DIFFERENT accounts
# must not share one, or each restore would hand the other's entries back.

ACCOUNT_FILES=(passwd group shadow gshadow)
IMAGE_ACCOUNTS=/run/usersync/image-accounts # the image's own entries, as shipped

# Only entries the image does not ship are cached and restored, and the image
# wins on a name clash — so an image update can change its system accounts
# without a stale copy overriding them.
snapshot_image_accounts() {
	install -d -m 0700 "$IMAGE_ACCOUNTS"
	local f
	for f in "${ACCOUNT_FILES[@]}"; do cp -a "/etc/$f" "$IMAGE_ACCOUNTS/$f"; done
}

# fields per line: passwd 7, group 4, shadow 9, gshadow 4
account_fields() {
	case $1 in passwd) echo 7 ;; group | gshadow) echo 4 ;; shadow) echo 9 ;; esac
}

restore_accounts() {
	[[ -n $ACCOUNTS_DIR ]] || return 0
	local f tmp n
	for f in "${ACCOUNT_FILES[@]}"; do
		if [[ ! -f $ACCOUNTS_DIR/$f ]]; then
			log "account cache: empty — creating every account (first start)"
			return 0
		fi
	done
	tmp=$(mktemp -d)
	for f in "${ACCOUNT_FILES[@]}"; do
		# A malformed cache is ignored as a whole, never half-applied.
		if ! awk -F: -v n="$(account_fields "$f")" 'NF != n || $1 == "" { bad = 1 } END { exit bad }' "$ACCOUNTS_DIR/$f"; then
			echo "WARNING: account cache $ACCOUNTS_DIR/$f is malformed; ignoring the cache" >&2
			rm -rf "$tmp"
			return 0
		fi
		awk -F: 'NR == FNR { seen[$1] = 1; print; next } !($1 in seen)' "/etc/$f" "$ACCOUNTS_DIR/$f" >"$tmp/$f"
	done
	# `cat >` rather than mv: keeps each file's owner and mode (shadow is root:shadow 0640).
	for f in "${ACCOUNT_FILES[@]}"; do cat "$tmp/$f" >"/etc/$f"; done
	rm -rf "$tmp"
	n=$(awk -F: 'NR == FNR { seen[$1] = 1; next } !($1 in seen)' "$IMAGE_ACCOUNTS/passwd" /etc/passwd | wc -l)
	log "account cache: restored $n accounts"
}

save_accounts() {
	[[ -n $ACCOUNTS_DIR ]] || return 0
	local f
	# shadow-utils holds <file>.lock while it rewrites; a copy taken then could
	# pair a new passwd with an old shadow. Skip and catch it on the next pass.
	for f in "${ACCOUNT_FILES[@]}"; do [[ -e /etc/$f.lock ]] && return 0; done
	for f in "${ACCOUNT_FILES[@]}"; do
		awk -F: 'NR == FNR { seen[$1] = 1; next } !($1 in seen)' "$IMAGE_ACCOUNTS/$f" "/etc/$f" >"$ACCOUNTS_DIR/.$f.new" || return 0
		if cmp -s "$ACCOUNTS_DIR/.$f.new" "$ACCOUNTS_DIR/$f"; then
			rm -f "$ACCOUNTS_DIR/.$f.new"
		else
			mv -f "$ACCOUNTS_DIR/.$f.new" "$ACCOUNTS_DIR/$f"
		fi
	done
}

# Before `usersync apply`: remember what the image ships, then put the cached
# accounts back.
accounts_cache_init() {
	[[ -n $ACCOUNTS_DIR ]] || return 0
	install -d -m 0700 "$ACCOUNTS_DIR"
	snapshot_image_accounts
	restore_accounts
}

# Keep the cache in step with what `watch` applies. It polls rather than hooking
# into watch: a change missed here is only a slower next start.
accounts_cache_follow() {
	[[ -n $ACCOUNTS_DIR ]] || return 0
	(while sleep 10; do save_accounts || true; done) &
}
