#!/usr/bin/env bash
# Verify, against a real smbd, what mode a file and a directory created over SMB
# actually land on — and which of the mask/force directives is doing the work.
#
# This exists because the arithmetic is easy to get wrong from first principles.
# Samba computes  mode = (base & mask) | force , and `base` is NOT a client
# request: SMB carries no unix mode, so smbd derives it from the DOS attributes
# in unix_mode(). Assuming a plausible-looking base (say 0644, as a Windows file
# "looks like") produces a confident and wrong conclusion about whether team
# files come out group-writable.
#
# Run:  docker run --rm -i debian:13-slim bash -s < scripts/verify-samba-modes.sh
#
# Expected output, and what internal/smbconf encodes:
#
#   parent 2770 (normal)
#     bare         file 0744   dir 2755     <- global defaults 0744/0755; only
#                                              consistent with base 0766/0777
#     mask-only    file 0660   dir 2770     <- the mask alone already suffices
#     with-force   file 0660   dir 2770     <- force adds nothing here
#
#   parent 0770 (setgid lost)
#     mask-only    dir 0770  setgid no      <- a mask can only CLEAR bits
#     with-force   dir 2770  setgid YES     <- force is the only thing that repairs it
#
# So: keep `force directory mode`, drop `force create mode`.
set -euo pipefail

export DEBIAN_FRONTEND=noninteractive
apt-get update -qq >/dev/null 2>&1
apt-get install -y -qq samba smbclient >/dev/null 2>&1

cat >/etc/samba/smb.conf <<'EOF'
[global]
   workgroup = WORKGROUP
   security = user
   passdb backend = tdbsam
   log level = 0

[bare]
   path = /srv/bare
   read only = no

[mask-only]
   path = /srv/mask-only
   read only = no
   create mask = 0660
   directory mask = 2770

[with-force]
   path = /srv/with-force
   read only = no
   create mask = 0660
   force create mode = 0660
   directory mask = 2770
   force directory mode = 2770
EOF

useradd -M -u 3001 -s /usr/sbin/nologin alice
groupadd -g 10001 team-a
usermod -aG team-a alice
printf 'pw!Xy123\npw!Xy123\n' | smbpasswd -a -s alice >/dev/null
smbpasswd -e alice >/dev/null

PW='pw!Xy123'
run_case() { # $1 = parent mode
	rm -rf /srv/*
	for d in bare mask-only with-force; do
		mkdir -p "/srv/$d"
		chgrp team-a "/srv/$d"
		chmod "$1" "/srv/$d"
	done
	pkill smbd 2>/dev/null || true
	sleep 1
	smbd -D
	sleep 3
	echo hello >/tmp/f.txt
	for share in bare mask-only with-force; do
		smbclient "//127.0.0.1/$share" -U "alice%$PW" \
			-c "put /tmp/f.txt file.txt; mkdir sub" >/dev/null 2>&1 || true
	done
	printf '  %-12s %-10s %-10s %s\n' SHARE FILE DIR SETGID
	for share in bare mask-only with-force; do
		f=$(stat -c '%04a' "/srv/$share/file.txt" 2>/dev/null || echo ----)
		d=$(stat -c '%04a' "/srv/$share/sub" 2>/dev/null || echo ----)
		case "$d" in 2*) s=YES ;; *) s=no ;; esac
		printf '  %-12s %-10s %-10s %s\n' "$share" "$f" "$d" "$s"
	done
}

echo "smbd $(smbd --version)"
echo
echo "=== parent 2770 (normal) ==="
run_case 2770
echo
echo "=== parent 0770 (setgid lost) ==="
run_case 0770
