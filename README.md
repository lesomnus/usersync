# usersync

Declarative reconciler for SMB-only users and groups. Declare the desired users
and groups in a version-controlled `roster.yaml`; `usersync` converges the system
to that declaration idempotently — creating unix accounts (`useradd`/`groupadd`),
setting SMB-only access (nologin + locked unix password + `smbpasswd`), and
managing group folders — with a strict safety model and no dependencies beyond a
single static Go binary.

See [`plan.md`](./plan.md) for the full design.

## Model

`roster.yaml` is the single source of truth. The loop is **edit → `plan` → `apply`**.

```yaml
# roster.yaml — desired state
groups:
  - { name: team-a, gid: 10001, description: Perception team }
users:
  - { name: skim, uid: 3001, full_name: Sunghyun Kim, groups: [team-a] }
  - { name: park, uid: 3004, status: disabled }   # SMB off, home + uid kept
  - { name: oldhand, uid: 3005, status: reserved } # no account; uid burned so it is never reused
```

Ids live in a reserved band: **uid 3000–9999** (users, each with a UPG whose gid
equals its uid) and **gid 10000–19999** (team groups). The band is what a future
on-prem AD has to carry verbatim as RFC2307 `uidNumber`/`gidNumber` — see
[`identity-roadmap.md`](./identity-roadmap.md).

`usersync.yaml` holds operational settings (paths, the managed id window, the
protected id ranges, seed, backend). The schema mirrors
[`proto/usersync/roster.proto`](./proto/usersync/roster.proto) and stays
protojson-compatible.

## Commands

```
usersync plan            # dry-run: collect state, diff against roster, print actions (no changes)
usersync plan --commands # also print the exact backend commands each action would run
usersync apply           # execute the actions (root; idempotent; never deletes)
usersync export          # print the current managed state as a roster.yaml (bootstrap / drift)
usersync export --format csv    # the RFC2307 id assignments, for seeding a directory
usersync export --format ldif --base-dn 'OU=X,DC=corp,DC=example,DC=com'
usersync detach <user>   # release the LOCAL account, keep the home (hand the name to AD)
usersync purge <user>    # DANGEROUS: archive home, delete account + UPG, reserve the uid
usersync shares          # print the smb.conf [homes]+[<team>] block from the roster
usersync shares --write  # splice it into smb.conf (testparm-validated, .bak kept); --reload reloads smbd
usersync passwd <user>   # print a user's seed-derived initial SMB password (to deliver / reset to)
usersync validate        # static-check config + roster (no root, no system access) — a CI/pre-commit gate
```

Note: `xli` expects flags before positional args, so put every flag ahead of the
positional user, e.g. `usersync passwd --seed-file s user` or `usersync purge --yes <user>`.

The account backend is auto-detected (`provider: auto`): shadow-utils
(`useradd`), busybox (`adduser`), or BSD `pw` — set `provider:` to pin one.

Common flags: `--roster`, `--config`, `--json`, `--skip-out-of-scope`,
`--seed-file`, `--home-base`, `--groups-base`.

Bootstrapping an already-configured server, then staying declarative:

```sh
usersync export > roster.yaml     # absorb current state
usersync plan                     # should show zero actions
$EDITOR roster.yaml               # make changes
usersync plan --commands          # review
usersync apply                    # converge
```

## Lifecycle (`status`) and uid reuse

Don't delete a user to offboard them — deleting frees the uid, and reusing a uid
later hands the old owner's files (by numeric uid) to a new person. Instead keep
the entry and set its `status`:

| status               | account        | SMB      | home | uid reserved |
| -------------------- | -------------- | -------- | ---- | ------------ |
| `active` (default)   | present        | enabled  | kept | yes          |
| `disabled`           | present, locked| disabled | kept | yes          |
| `reserved`           | none           | none     | —    | yes          |

Uniqueness of names and uids is enforced across all statuses, so a `reserved`
tombstone permanently blocks its uid from reuse.

## Carrying the numbers into a directory

Files are owned by a numeric uid, and on a snapshotting filesystem the historical
ones cannot be chown'd at all — so if a directory service invents its own numbers
when it takes over, every snapshot ends up pointing at an identity that no longer
matches. `export --format csv|ldif` renders the assignments already in use so
they can be seeded into AD's RFC2307 `uidNumber`/`gidNumber` attributes instead,
which makes the handover a no-op for every file:

```sh
usersync export --format csv > ids.csv
```
```powershell
Import-Csv ids.csv | Where-Object type -eq 'group' |
  ForEach-Object { Set-ADGroup $_.name -Replace @{gidNumber=[int]$_.gid_number} }
Import-Csv ids.csv | Where-Object type -eq 'user'  |
  ForEach-Object { Set-ADUser  $_.name -Replace @{uidNumber=[int]$_.uid_number; gidNumber=[int]$_.gid_number} }
```

CSV identifies accounts by name (`sAMAccountName`) and lets PowerShell resolve
the DN; LDIF has to build one, and assumes each account's CN equals its name,
which AD does not guarantee. Prefer CSV unless you have checked. Reserved
tombstones are skipped by both — they have no account to seed, and their uids
stay blocked by the reservation of the whole band.

## Handing a user over to a directory service

`detach` is the migration counterpart of `purge`: it deletes the local unix
account, its UPG and its tdbsam entry, but leaves the home directory and every
file in it alone, so winbind/AD can answer for that name instead. It refuses
unless the roster still declares the user — that entry keeps the uid reserved and
makes the step reversible, since `usersync apply` recreates the local account.

After releasing the account it looks the name up again and **fails** if it now
resolves to a different uid: the files are owned by a number, so a name that
comes back pointing somewhere else means the data and the identity have come
apart. See [`identity-roadmap.md`](./identity-roadmap.md).

## Safety

- **Protected ids are untouchable**: any uid/gid below `system_floor` (default and
  minimum **1000** — it can be raised but never lowered) or inside a `protect`
  range is never created, modified, disabled, or deleted, and declaring one is a
  hard error.
- **`apply` never deletes** — absence from the roster disables SMB (home kept).
  Deletion happens only via `purge`.
- **Out-of-scope entries** (neither managed nor protected) are refused by default;
  `on_out_of_scope: skip` / `--skip-out-of-scope` warns and skips them instead.
- **Passwords** are seed-derived and set only at creation, so a user's own change
  is preserved. The seed is never stored in the roster (`seed.secret` or
  `USERSYNC_SEED`).

## Build & test

```sh
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o usersync .   # static binary
go build ./...          # compile check
go test ./...           # the pure core (idrange/reconcile/roster/secret) needs no root
```
