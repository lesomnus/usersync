# usersync

Declarative reconciler for SMB-only users and groups. Declare the desired users
and groups in a version-controlled `roster.yaml`; `usersync` converges the system
to that declaration idempotently — creating unix accounts (`useradd`/`groupadd`),
setting SMB-only access (nologin + locked unix password + `smbpasswd`), and
managing group folders — with a strict safety model and no dependencies beyond a
single static Go binary.

See [`plan.md`](./plan.md) for the full design and
[`identity-roadmap.md`](./identity-roadmap.md) for why one password backs both
the web and SMB paths, and what changes when an on-prem AD arrives.

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
protected id ranges, seed, backend). Both files are decoded **strictly** — an
unknown key is an error, not a silent fallback to the safe-looking default.

`roster.yaml`'s shape is mirrored by
[`proto/usersync/roster.proto`](./proto/usersync/roster.proto) and stays
protojson-compatible. (No codegen is wired up yet; the `gen/` path in its
`go_package` is aspirational.)

## Commands

```
usersync plan            # dry-run: collect state, diff against roster, print actions (no changes)
usersync plan --commands # also print the exact backend commands each action would run
usersync apply           # execute the actions (root; idempotent; never deletes)
usersync export          # print the current managed state as a roster.yaml (bootstrap / drift)
usersync export --format csv    # the RFC2307 id assignments, for seeding a directory
usersync export --format ldif --base-dn 'OU=X,DC=corp,DC=example,DC=com'
usersync audit           # read-only: does what the system resolves match the roster?
usersync detach <user>   # release the LOCAL account, keep the home (hand the name to AD)
usersync purge <user>    # DANGEROUS: archive home, delete account + UPG, reserve the uid
usersync shares          # print the smb.conf [homes]+[<team>] block from the roster
usersync shares --write  # splice it into smb.conf (testparm-validated, .bak kept); --reload reloads smbd
usersync passwd <user>   # print a user's seed-derived initial SMB password (to deliver / reset to)
usersync validate        # static-check config + roster (no root, no system access) — a CI/pre-commit gate
usersync roster          # print the DECLARED roster as JSON (vs `export`, which scans the system)
usersync member add <user> <team>      # edit team membership IN the roster (no system change)
usersync member remove <user> <team>   #   preserves comments and layout; run `apply` after
usersync detach --keep-upg <user>   # recommended form: leaves the UPG so `ls -l` still names the group
usersync config          # print the effective configuration after defaults
usersync version         # print the build stamp
```

Note: `xli` expects flags before positional args, so put every flag ahead of the
positional user, e.g. `usersync passwd --seed-file s user` or `usersync purge --yes <user>`.

The account backend is auto-detected (`provider: auto`): shadow-utils
(`useradd`), busybox (`adduser`), or BSD `pw` — set `provider:` to pin one.

`--config` is a **root** flag and goes before the subcommand:
`usersync --config alt.yaml plan`. Putting it after (`usersync plan --config …`)
is rejected.

Per-command flags: `--roster`, `--skip-out-of-scope`, `--home-base`,
`--groups-base` on every roster-reading command; `--json` on `plan`/`apply`/`audit`;
`--seed-file` on `plan`/`apply`/`passwd`. A command only accepts a flag it
actually reads — `export --json` is an error, not a no-op.

`detach` also takes `--keep-upg` (keep the user's private group locally),
`--keep-smb` (leave the SMB account alone), and `--yes`.

Bootstrapping an already-configured server, then staying declarative:

```sh
usersync export > roster.yaml     # absorb current state (ONE TIME — see below)
usersync plan                     # should show zero actions
$EDITOR roster.yaml               # make changes
usersync plan --commands          # review
usersync apply                    # converge
```

> `export` reads the existing `roster.yaml` to carry forward what no scan can
> see: group descriptions, and `status: reserved` tombstones (a reserved user
> has no account by definition). But `export > roster.yaml` makes the **shell**
> truncate the file before usersync opens it, so that one form still loses them.
> After the initial bootstrap, export somewhere else and diff:
> `usersync export > roster.new.yaml && diff roster.yaml roster.new.yaml`.

### Team owners

A group may declare `owners`: the people allowed to add and remove its members.

```yaml
groups:
  - name: team-a
    gid: 10001
    owners: [skim]
```

This is not a usersync invention. POSIX keeps group administrators in the third
field of `/etc/gshadow`, `gpasswd -A` is what writes them, and an administrator
there can run `gpasswd -a`/`-d` on that group **without being root**. usersync
applies the field, so `getent gshadow` and the roster agree and `plan` reports a
disagreement instead of the roster asserting something nothing enforces. An
on-prem AD later carries the same fact as the group's `managedBy`.

Two things it does not do:

- **The roster still owns the membership list.** An owner who runs `gpasswd -a`
  adds a member, and the next `apply` takes them back out, because
  `users[].groups` is the desired state and apply replaces the set exactly. The
  durable path is an edit to the roster — see `usersync member` below.
- **It grants nothing where users cannot log in.** With `/usr/sbin/nologin` and a
  locked unix password, no member can run `gpasswd` at all, so the gshadow entry
  is a record rather than an access control. The enforcement is whatever front
  end people actually reach.

Every name must be a user declared in the same roster. Only shadow-utils has
gshadow; on busybox and pw the field is reported as unsupported rather than
silently dropped.

### Editing membership from a program

`usersync member` is for callers that are not a person — a web UI, a script. It
edits `roster.yaml` through the document's syntax tree rather than by decoding
and re-encoding it, so comments, blank lines and `groups: [team-a]` flow style
all survive and a membership change shows up as **one changed line**. Decoding
and re-encoding the shipped roster rewrites 17 of its 46 lines, which is not
data loss but is the difference between a reviewable change and a file that
says everything changed.

The edited roster is loaded and validated **before** it is written, so an edit
that would make the roster unloadable is refused rather than saved — otherwise a
bad call would be discovered at the next boot, by a server that will not start.

It changes the declaration only. `usersync apply` still converges the system,
which leaves room for `plan` in between exactly as there is for a hand edit.

One caveat, once: the printer emits a single space before a trailing comment, so
the first machine write re-aligns hand-aligned inline comments. Subsequent
writes do not.

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

## After a directory takes over: `mode: audit`

Setting `mode: audit` in `usersync.yaml` says the accounts belong to something
else now. `apply` and `purge` refuse — an apply would try to recreate what the
directory owns — while `detach` and `shares` stay available, because releasing
local ownership moves *towards* that state and smb.conf is not an account.

What usersync keeps doing is the part nothing else does: the roster is still the
ledger of which number belongs to whom, and `usersync audit` checks that the
directory agrees with it. It is read-only, needs no root, and exits non-zero on
any disagreement, so it runs from cron:

```
AUDIT (roster vs. what the system resolves)
  ✗ group team-a           declared 10001, but resolves to 19999 — files stay on 10001
  ✗ user  ghost            declared 3009, but the name does not resolve
  ✗ user  intruder         resolves to 3007 inside the managed band but is not in the roster
  ✗ user  park             declared 3004, but resolves to 100042 — files stay on 3004
Summary: 4 users, 1 groups checked — 4 findings
         (undeclared/collision checks saw 5 users and 4 groups in the enumeration)
```

It also reports a reserved tombstone that has come back to life, and two names
that resolve to the same number — neither of which the roster's own uniqueness
check can see, because it validates what is *declared*, not what the directory
*answers*.

Declared entries are checked with a **keyed** lookup (one `getent passwd <name>`
each), because winbind does not enumerate domain accounts unless
`winbind enum users = yes` — reading them out of the enumeration would report
every handed-over user as missing. The undeclared/collision sweep necessarily
does come from the enumeration, so it sees local accounts and not the directory;
the run prints how many names that was, so a clean result is not read as proof
that nothing else is out there.

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

## License

Apache-2.0 — see [LICENSE](./LICENSE).
