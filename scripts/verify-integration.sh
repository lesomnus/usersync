#!/usr/bin/env bash
#
# End-to-end integration verification for usersync.
#
# Runs the REAL binary as root against REAL shadow-utils and Samba (smbpasswd/
# pdbedit) inside a THROWAWAY container that is removed on exit — nothing is
# created on the host or the devcontainer. Requires a reachable docker daemon
# (the .devcontainer docker-in-docker feature provides one).
#
#   bash scripts/verify-integration.sh
#
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$here"
img="${VERIFY_IMAGE:-debian:trixie}"

# --- ensure a docker daemon is reachable (start dind's dockerd if needed) ---
if ! docker info >/dev/null 2>&1; then
	echo ">> starting dockerd (docker-in-docker)..."
	sudo bash -c 'nohup dockerd >/tmp/dockerd.log 2>&1 &' || true
	for _ in $(seq 1 30); do docker info >/dev/null 2>&1 && break; sleep 1; done
fi
docker info >/dev/null 2>&1 || { echo "FATAL: no docker daemon reachable (see /tmp/dockerd.log)"; exit 1; }

# --- build the static binary on the devcontainer (has Go) ---
echo ">> building static usersync..."
CGO_ENABLED=0 go build -trimpath -o dist/usersync .

# --- the in-container test driver ---
drv="$(mktemp)"
trap 'rm -f "$drv"' EXIT
cat > "$drv" <<'SCRIPT'
set -eu
export DEBIAN_FRONTEND=noninteractive
echo ">> installing shadow-utils + samba in the throwaway container..."
apt-get update -qq
apt-get install -y -qq samba samba-common-bin passwd >/dev/null

mkdir -p /research/home /research/groups /work
cd /work
printf 'integration-seed\n' > seed.secret; chmod 600 seed.secret

cat > usersync.yaml <<EOF
paths: { home: /research/home, groups: /research/groups }
manage: { uid: { min: 3000, max: 6999 }, gid: { min: 7000, max: 7999 } }
protect: { system_floor: 1000 }
provider: shadow-utils
EOF
cat > roster.yaml <<EOF
groups:
  - { name: team-a, gid: 7001 }
users:
  - { name: skim, uid: 3001, full_name: Sunghyun Kim, groups: [team-a] }
  - { name: park, uid: 3004, status: disabled }
EOF

US="usersync --config usersync.yaml"
pass=0; fail=0
check(){ if eval "$2"; then echo "  PASS: $1"; pass=$((pass+1)); else echo "  FAIL: $1"; fail=$((fail+1)); fi; }
# acct_flags <user> -> the bracketed Account Flags string from pdbedit, e.g. [U          ]
acct_flags(){ pdbedit -Lv 2>/dev/null | awk -v u="$1" '/^Unix username:/{c=$3} /^Account Flags:/&&c==u{print;exit}'; }

echo "== apply =="
$US apply --roster roster.yaml --seed-file seed.secret

echo "== account checks =="
check "user skim exists with uid 3001"       '[ "$(id -u skim)" = 3001 ]'
check "UPG: primary gid == uid (3001)"        '[ "$(id -g skim)" = 3001 ]'
check "supplementary group team-a"            'id -nG skim | tr " " "\n" | grep -qx team-a'
check "home is 0700 skim:skim"                '[ "$(stat -c "%a %U:%G" /research/home/skim)" = "700 skim:skim" ]'
check "login shell is nologin"                'getent passwd skim | grep -q ":/usr/sbin/nologin$"'
check "unix password is locked (L)"           'passwd -S skim | awk "{print \$2}" | grep -q "^L"'
check "SMB account present + enabled"         'acct_flags skim | grep -q "\[U"'
check "gecos preserved"                       'getent passwd skim | cut -d: -f5 | grep -qx "Sunghyun Kim"'
check "group folder is 2770 setgid, team-a"   '[ "$(stat -c "%a %G" /research/groups/team-a)" = "2770 team-a" ]'
check "park created but SMB DISABLED"         'acct_flags park | grep -q "D"'
check "park home exists"                      '[ -d /research/home/park ]'

echo "== idempotency: re-plan on the same roster =="
out="$($US plan --roster roster.yaml --seed-file seed.secret 2>/dev/null)"; echo "$out" | sed 's/^/  /'
check "steady plan has no change actions"     '! grep -qE "create-|update-|add-smb|enable-|disable-|ensure-home" <<<"$out"'

echo "== uid reuse is blocked =="
cat > reuse.yaml <<EOF
users:
  - { name: park, uid: 3004, status: reserved }
  - { name: intruder, uid: 3004 }
EOF
if $US plan --roster reuse.yaml >/dev/null 2>&1; then
	echo "  FAIL: reusing a reserved uid was not blocked"; fail=$((fail+1))
else
	echo "  PASS: reusing a reserved uid is rejected at load"; pass=$((pass+1))
fi

echo "== protected/out-of-range hard guard =="
printf 'users:\n  - { name: sys, uid: 500 }\n' > prot.yaml
if $US plan --roster prot.yaml >/dev/null 2>&1; then
	echo "  FAIL: uid 500 (below floor) was not refused"; fail=$((fail+1))
else
	echo "  PASS: uid 500 (below floor) is hard-refused"; pass=$((pass+1))
fi

echo "== offboarding: drop skim from roster => SMB disabled, data kept =="
printf 'groups:\n  - { name: team-a, gid: 7001 }\n' > empty.yaml
$US apply --roster empty.yaml --seed-file seed.secret >/dev/null
check "skim home preserved after removal"     '[ -d /research/home/skim ]'
check "skim account NOT deleted"              'id -u skim >/dev/null 2>&1'
check "skim SMB now disabled"                 'acct_flags skim | grep -q "D"'

echo "== re-onboard: add skim back => re-enabled =="
$US apply --roster roster.yaml --seed-file seed.secret >/dev/null
check "skim SMB re-enabled"                   'acct_flags skim | grep -q "\[U"'

echo
echo "RESULT: $pass passed, $fail failed"
[ "$fail" = 0 ]
SCRIPT

echo ">> running integration test in a throwaway $img container..."
docker run --rm \
	-v "$here/dist/usersync:/usr/local/bin/usersync:ro" \
	-v "$drv:/drv.sh:ro" \
	"$img" bash /drv.sh
