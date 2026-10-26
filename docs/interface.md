# The interface usersync offers

usersync turns `roster.yaml` into system state. Other programs then build on that
state, and on a few of usersync's commands, without importing any of its code.
This page is the list of what they may rely on. Everything not listed here is an
implementation detail and may change without notice.

Today's consumers:

| Consumer | What it uses |
|---|---|
| [darak](https://github.com/lesomnus/darak) | the on-disk layout (it opens files as the user, and routes a reader to their view), `roster`, `member`, `apply`, `audit --json`, `export --format csv`, `passwd` |
| Samba | the share block `shares --write` splices into smb.conf |
| an AD import (PowerShell) | `export --format csv` |

## Rules for changing it

- **Adding is compatible.** A new field in JSON output, a new column at the
  *end* of the CSV, a new command or a new optional flag. A consumer must ignore
  what it does not know.
- **Everything else is a break.** Renaming or removing a field, column, command
  or flag; changing what a value means; moving something on disk. A break bumps
  the version that covers it (`schema` in `roster`'s output) and is called out in
  the release notes, so a consumer can refuse a version it does not understand
  rather than misread it.
- **This page and the tests change together.** Each section names the test that
  pins it. A change that makes one of those tests fail is a change to this page.

Where the layout below says `<home>` and `<groups>`, it means `paths.home` and
`paths.groups` from `usersync.yaml` (for example `/srv/data/homes` and
`/srv/data/teams`).

## 1. On-disk layout

| Path | What | Owner | Mode |
|---|---|---|---|
| `<home>/<user>` | a user's home, if their profile has `home: true` (the default) | `<uid>:<uid>` (the user's private group) | `0700` |
| `<groups>/<team>` | a team's folder, one per group | group = the team's gid; the owning **user is not managed** (root when usersync created it) | `2770`; `2775` with `anonymous: read`; `2777` with `anonymous: write` |
| `<groups>/<reader>/<team>` | a reader group's read-only **view** of `<team>` (see 1.1) | as `<team>`, seen through the mapping | as `<team>`, read-only |

- Team folders carry the setgid bit, so what is created inside takes the team's
  group. Files are meant to be `0660` (`0664`/`0666` with anonymous access);
  usersync sets this through the SMB masks (section 2), not by walking the tree.
- usersync never walks a team's tree to set permissions. Granting or revoking a
  reader costs one mount, whatever the folder holds.

Pinned by: `internal/fsops` (`TestViewPath`, `TestSplitViewPath`), `internal/smbconf`.

### 1.1 Reader views

`readers: [r]` on group `t` is enforced by a mount, not by anything written on
the files:

- **Where:** `<groups>/<r>/<t>`, inside the reader group's own folder. The team's
  own folder `<groups>/<t>` stays closed to `r`: its members are not in `t`.
- **What:** a read-only, id-mapped, recursive bind of `<groups>/<t>`
  (`mount --rbind -o ro,X-mount.idmap=…`). Through it, files owned by `t`'s gid
  appear owned by `r`'s gid, so a reader reads with ordinary mode bits. Every
  write is refused by the kernel with `EROFS`.
- **The mountpoint** is an empty, root-owned `0555` directory that usersync
  creates. usersync refuses to mount over a non-empty directory. It also refuses
  an empty one that it did not make (not root-owned `0555`), because that folder
  is somebody's in the reader group's folder.
- **A view is identified by the mount, not the path.** Only a read-only
  id-mapped mount at `<groups>/<r>/<t>` counts as a view. Any other mount with
  that shape is left alone. If one sits where a declared view must go, the grant
  is refused rather than mounted over it.
- **Revocation** is the unmount. The mount table is the only record, so a reader
  removed from the roster is revoked at the next apply.

A consumer that serves files as the user, as darak does, must open the view for
a reader of `t` who is not a member of `t`. It takes the view's path from
`roster`'s `views` (section 3) rather than building it. Opening
`<groups>/<t>/…` instead fails closed (`EACCES`). It cannot grant anything.

**Constraint:** a group whose folder holds views (a group that is someone's
reader) cannot itself be given readers. The recursive bind would include those
views, and an id-mapped mount cannot be id-mapped again (`mount_setattr(): cannot
set ID-mapping: Operation not permitted`). Today `validate` does not catch this,
and `apply` fails. See [#13](https://github.com/lesomnus/usersync/issues/13).

## 2. SMB shares

`shares` renders, and `shares --write` splices between markers into smb.conf:

- `[homes]`: path `<home>/%U`, `valid users = %S`, masks `0600`/`0700`.
- `[<team>]` for every group, sorted by name: path `<groups>/<team>`, comment
  from `description`, `force group = <team>`, masks matching section 1.
  - Private (`anonymous: none`): `valid users = @<team>`. **Readers are not
    admitted here.** They see the team inside their own group's share, through
    the view.
  - Anonymous (`read`/`write`): any account may connect, `guest ok = no`. Access
    without an account is web-only.

The share name is the group name. Pinned by: `internal/smbconf`,
`TestCLISharesRendersDescriptionAsComment`.

## 3. Commands

### `usersync roster`: the declaration, as JSON

Prints the roster as declared, not the system state (that is `export`), on
stdout. Logs go to stderr. The exit code is non-zero if the roster does not load.
An authorization decision should rest on this output: it is what an operator
reviewed and committed.

```json
{ "schema": 1, "paths": { "home": "…", "groups": "…" }, "groups": [ … ], "users": [ … ] }
```

| Field | Type | Meaning |
|---|---|---|
| `schema` | int | Version of this shape. Currently `1`. Refuse a number you do not know. |
| `paths.home` / `paths.groups` | string | the configured bases (`<home>`, `<groups>` above) |
| `groups[].name` / `gid` | string / uint32 | |
| `groups[].description` | string, omitted if empty | display only |
| `groups[].owners` | string[] | users who may change the membership |
| `groups[].readers` | string[] | groups granted a read-only view of this group's folder (1.1), **as declared** |
| `groups[].views` | `{team, path}[]` | the other side: the teams **this** group may read, sorted by team, each with the absolute path apply mounts its view at. A consumer opens `path` as given. |
| `groups[].members` | string[] | for an ordinary group, as declared (a disabled user is still listed). For an `all` group, the **resolved cohort**: active users of that profile. A consumer never expands `all` itself. |
| `groups[].all` | bool, omitted if false | a display hint that `members` is a cohort |
| `groups[].anonymous` | `"none"` \| `"read"` \| `"write"` | unauthenticated (web) access to the folder |
| `users[].name` / `uid` | string / uint32 | |
| `users[].full_name` | string, omitted if empty | |
| `users[].status` | `"active"` \| `"disabled"` \| `"reserved"` | |
| `users[].home` | bool | whether `<home>/<user>` is provisioned |
| `users[].quota` | uint64 bytes, omitted if unlimited | `0` is a limit (no space), not "unlimited" |

Also guaranteed: groups and users come out in roster order, and list fields are
`[]`, never `null`. Only entries this installation manages are printed
(out-of-range ids are skipped with a warning on stderr).

Pinned by: `internal/integration` `TestInterfaceRosterOutput`, byte for byte.

### `usersync member add|remove <user> <group> [--json]` and `usersync apply`

`member` edits `roster.yaml` in place and does **not** apply it. It touches only
the line that declares the membership and keeps comments and layout. It
refuses to write an edit that leaves a roster that does not load or validate,
such as a name the roster does not declare. `apply` converges the system to the
roster. darak runs them in that order.

### `usersync audit --json`

Compares the roster with what the system resolves (NSS included). It changes
nothing and needs no root. It exits non-zero when there are findings.

```json
{ "users_checked": 0, "groups_checked": 0, "enumerated_users": 0, "enumerated_groups": 0,
  "findings": [ { "kind": "user|group", "name": "", "code": "", "want": 0, "got": 0, "detail": "" } ] }
```

`code` is one of `missing`, `id-mismatch`, `tombstone-live`, `undeclared`,
`collision`. `want`, `got` and `detail` are omitted when they do not apply.

### `usersync export --format csv`

What the system has, not the declaration. The header is fixed:
`type,name,uid_number,gid_number,unix_home_directory,login_shell`. `type` is
`group` or `user`. A group row leaves the uid, home and shell columns empty.

### `usersync passwd <user>`

Prints the user's deterministic initial SMB password, derived from the seed. It
needs only the seed (`--seed-file` or `USERSYNC_SEED`).

## Not part of the interface

- The YAML schema of `roster.yaml` is documented in the README and in
  `proto/usersync/roster.proto`. It is a file people edit, and it can move
  independently of the JSON above.
- Log lines, the wording of messages and `plan`'s text output.
- Anything under `internal/`. Consumers do not import it, and Go stops them.

## Known gaps

- **Views live inside data folders.** `<groups>/<r>/<t>` shares a namespace
  with what the reader group's members put in their own folder, and it is the
  cause of the reader-group constraint in 1.1. The direction is to move views to
  a namespace only usersync owns, `<views>/<r>/<t>`
  ([#13](https://github.com/lesomnus/usersync/issues/13)). A consumer that reads
  `views[].path` instead of building the path follows that move without a change.
- The reader-group constraint in 1.1 is enforced at apply time, not at load.
