# usersync 운영 가이드 (admin-guide)

> 파일 서버의 SMB 사용자/그룹을 **선언 파일(`roster.yaml`) 편집 → `usersync apply`** 로 관리하는 실무 절차.
> 설계 배경·근거는 [`plan.md`](./plan.md), 사용 요약은 [`README.md`](./README.md).

---

## 0. 정신 모델 (딱 이것만 기억)

- **`roster.yaml`이 유일한 진실.** 시스템을 손으로 만지지 말고 **선언만 편집**한다.
- 반복 리듬: **편집 → `usersync plan`(미리보기, 무변경) → `usersync apply`(수렴).**
- `apply`는 **절대 삭제하지 않는다**(비활성까지만). 완전 삭제는 `usersync purge`만.
- **UID/GID < 1000 및 예약 대역은 불가침** — 설정으로도 못 건드린다.
- 모든 변경은 Git 커밋으로 남긴다 → 계정 관리 이력 = git 이력.

접근 모델: 연구원은 **SMB로 자기 홈만** 쓰고 **SSH/콘솔 로그인은 불가**(nologin 셸 + 유닉스 비번 잠금). SMB 비번만 별도(tdbsam).

---

## 1. 최초 셋업 (한 번만)

### 1.1 바이너리 배치
정적 단일 바이너리다. 빌드(`CGO_ENABLED=0 go build -o usersync .`) 후 파일 서버에 복사하거나, 컨테이너 이미지(`ghcr.io/lesomnus/usersync`)를 쓴다.

### 1.2 시드 생성 (초기 비번 파생용)
초기 SMB 비번은 시드에서 **결정적으로 파생**된다. 시드는 roster에 넣지 않고 별도 파일/환경변수로 준다.
```sh
umask 077
openssl rand -base64 32 > seed.secret   # mode 0600
```
- `seed.secret`은 **절대 커밋 금지**(이미 `.gitignore`에 있음). 안전하게 백업해 둔다 — 잃어버리면 기존 초기 비번을 재계산할 수 없다.
- 또는 `USERSYNC_SEED` 환경변수로 주입 가능.

### 1.3 운영 설정 `usersync.yaml`
경로·관리 범위·보호 범위·provider를 정의한다. 기본값이 파일 서버 레이아웃과 맞다:
```yaml
paths:
  home: /research/home
  groups: /research/groups
manage:
  uid: { min: 3000, max: 6999 }   # 사용자(UPG gid=uid 동일 범위)
  gid: { min: 7000, max: 7999 }   # 팀 그룹
protect:
  system_floor: 1000              # 이 값 미만은 불가침(코드가 최소 1000 보장)
on_out_of_scope: error            # error | skip
seed_file: ./seed.secret
provider: auto                    # auto | shadow-utils | busybox | pw
```
스키마 상세는 [plan.md §4](./plan.md).

### 1.4 기존 상태 흡수 (부트스트랩)
파일 서버엔 이미 수동 계정이 있다. 백지에서 시작하지 말고 현재 상태를 `roster.yaml`로 뽑아낸다:
```sh
sudo ./usersync export > roster.yaml      # 관리 범위(3000+/7000+) 안의 실제 계정을 역출력
$EDITOR roster.yaml                        # 검토·정리
git add roster.yaml usersync.yaml && git commit -m "adopt usersync"
sudo ./usersync plan                       # action 0건이어야 정상(= 흡수 정합성 확인)
```
> plan.md §1 기준, 관리 범위 밖(3000 미만)의 테스트 계정(alice/bob 등)은 export에 **안 나온다**. 필요하면 수동 정리: `sudo smbpasswd -x alice; sudo userdel alice; sudo groupdel team-a` 등.

---

## 2. `roster.yaml` 형식 (요약)

```yaml
groups:
  - { name: team-a, gid: 7001, description: Perception team }
users:
  - name: skim
    uid: 3001
    full_name: Sunghyun Kim        # 표시용 실명(GECOS). 콤마·콜론 불가.
    groups: [team-a]               # 보조 그룹. 생략=홈만.
  - name: park
    uid: 3004
    status: disabled               # SMB off, 홈·계정·uid 유지 (휴직)
  - name: oldhand
    uid: 3005
    status: reserved               # 계정 없음, uid만 예약(재사용 차단)
```
- **1차 그룹은 UPG**: 툴이 사용자명과 같은 개인 그룹을 gid=uid로 만든다.
- uid/name은 **active/disabled/reserved 전체에 걸쳐 유일**해야 한다(= 재사용 방지).

---

## 3. 자주 하는 일 (Task 별)

> 모든 변경은 **plan으로 먼저 확인**하고 apply. `apply`/`purge`는 root 필요.

### 3.1 신규 연구원 온보딩
```yaml
# roster.yaml users: 에 추가
  - name: skim
    uid: 3001                      # 3000–6999 중 비어있는 값
    full_name: Sunghyun Kim
    groups: [team-a]
```
```sh
sudo ./usersync plan --commands    # 나갈 명령(useradd/usermod/smbpasswd …) 검토
sudo ./usersync apply              # 생성: UPG→useradd→홈(0700)→보조그룹→비번잠금→SMB등록·활성
./usersync passwd --seed-file seed.secret skim   # 초기 SMB 비번을 확인해 사용자에게 전달
```
- 사용자는 SMB로 `\\파일 서버\skim`(자기 홈) 접속. 서버 로그인은 불가.

### 3.2 팀 신설 / 팀 배정 변경
```yaml
groups:
  - { name: team-b, gid: 7002, description: Planning team }
users:
  - name: jlee
    uid: 3002
    groups: [team-a, team-b]       # 소속을 '정확한 집합'으로 선언
```
```sh
sudo ./usersync apply              # 그룹 생성+폴더(2770 setgid), 보조그룹 치환
sudo ./usersync shares --write --reload   # (선택) smb.conf에 [team-b] 공유 자동 추가
```
> `users[].groups`는 **치환**이다(추가 아님). roster에 적힌 집합이 곧 그 사용자의 전체 보조그룹.

### 3.3 오프보딩 (퇴사·휴직) — 데이터 보존, UID 예약
**항목을 지우지 말고 `status`만 바꾼다.** 지우면 uid 예약이 풀려 나중에 재사용→파일 오소유 사고가 난다.
```yaml
  - name: park
    uid: 3004
    status: disabled               # 휴직: SMB만 차단, 홈·계정 유지
```
```sh
sudo ./usersync apply              # smbpasswd -d 만. 홈 데이터 그대로.
```
- **복귀**: `status`를 지우면(=active) `apply` 시 SMB 재활성(비번·데이터 그대로).
- **영구 은퇴**(계정도 없애되 번호는 영구 예약): `status: reserved`.

### 3.4 비밀번호 전달 / 재설정
usersync는 **기존 비번을 자동 재설정하지 않는다**(사용자가 바꾼 비번 보존).
```sh
# 초기 비번 확인(온보딩 전달용, 또는 분실 시 되돌릴 값)
./usersync passwd --seed-file seed.secret skim

# 강제로 초기 비번으로 되돌리기 / 임의 재설정
sudo smbpasswd -a skim             # 대화형으로 새 비번 입력
```
> 플래그는 인자 앞: `usersync passwd --seed-file s skim` (§6 참고).

### 3.5 완전 삭제 (위험)
정말 계정·홈까지 없앨 때만. 홈은 먼저 아카이브된다.
```sh
sudo ./usersync purge --yes skim   # 홈 tar 백업 → smbpasswd -x → userdel -r → groupdel(UPG)
```
- purge는 uid 재사용 방지용 `status: reserved` **스니펫을 출력**한다 → roster에 붙여넣고 커밋(주석 보존). `--reserve`를 주면 roster 파일에 직접 기록하지만 주석/서식이 사라진다.
- 홈 아카이브가 실패하면 **삭제를 중단**한다(데이터 손실 방지). GECOS에 콜론이 있어도 홈 경로를 정확히 파싱한다.

### 3.6 정기 드리프트 점검
```sh
sudo ./usersync plan                       # 무변경이어야 정상. 뭔가 나오면 누가 손댄 것.
sudo ./usersync export | diff - roster.yaml   # 실제↔선언 델타
```
cron/CI로 주기 실행하면 좋다. `--json`으로 기계가독 출력.

---

## 4. SMB 공유(smb.conf) 관리

새 팀 그룹은 `smb.conf`에 `[team-x]` 공유가 있어야 SMB에 노출된다.
```sh
./usersync shares                  # 생성될 [homes]+[team-*] 블록을 미리보기(무변경)
sudo ./usersync shares --write     # smb.conf의 마커 블록에 반영(testparm 검증 + .bak 백업)
sudo ./usersync shares --reload    # 위 + smbd reload
```
- usersync는 `# >>> usersync-shares >>>` … `# <<< usersync-shares <<<` **마커 사이만** 관리한다. 그 밖의 수동 설정은 건드리지 않는다.
- testparm 검증에 실패하면 원본을 그대로 두고 중단한다.

---

## 5. 안전 규칙 (반드시 숙지)

| 규칙 | 의미 |
| --- | --- |
| **dry-run 우선** | 습관적으로 `plan` → 검토 → `apply`. |
| **삭제 없음** | `apply`는 비활성까지만. 삭제는 `purge`만. |
| **불가침 대역** | uid/gid < `system_floor`(≥1000)와 `protect` 범위는 어떤 경로로도 안 건드림. roster가 그런 id를 선언하면 하드 거부. |
| **범위 밖 처리** | manage에도 protect에도 안 드는 엔트리는 기본 거부. `--skip-out-of-scope`면 경고 후 그 줄만 건너뜀. |
| **멱등** | 같은 roster로 `apply` 반복 → 2회차부터 변경 0건. |
| **UID 재사용 금지** | 은퇴자는 지우지 말고 `disabled`/`reserved`로 남겨 번호를 예약. |

종료 코드: 성공 0, 거부(uid/gid 불일치 등 수동개입 필요) 비0 → CI/스크립트에서 감지 가능.

---

## 6. 자주 걸리는 것 (Troubleshooting)

- **`error: must run as root`** — `apply`/`purge`/`shares --write`는 root 필요. `sudo`로 실행.
- **`passwd`/`purge`에서 `flags ... at the behind of the arguments`** — 이 CLI는 **플래그를 positional 인자 앞에** 둬야 한다.
  - ✗ `usersync purge skim --yes`  ✓ `usersync purge --yes skim`
  - ✗ `usersync passwd skim --seed-file s`  ✓ `usersync passwd --seed-file s skim`
- **plan이 매번 `ensure-home`/`create-group`(folder)를 낸다** — 홈/그룹 폴더가 없거나 퍼미션이 어긋난 것. `apply`하면 `0700`/`2770 setgid`로 보정된다(멱등).
- **`... out of manage scope`로 거부됨** — 그 uid/gid가 `manage` 창 밖. 값을 고치거나, 무시하려면 `on_out_of_scope: skip`(또는 `--skip-out-of-scope`). 단 `< system_floor`/`protect`는 플래그로도 못 통과.
- **`duplicate uid`** — 다른(은퇴 포함) 항목이 같은 uid를 이미 점유. uid는 재사용 불가.
- **SMB 상태가 안 보임(비-root export)** — `pdbedit`는 root 필요. 비-root `export`는 유저/그룹만 출력하고 SMB 상태는 경고 후 생략한다.
- **provider 자동탐지 실패** — `provider: auto`가 `useradd`→`adduser`→`pw` 순으로 찾는다. 없으면 `provider:`로 명시.

---

## 7. 배포 체크리스트 (§plan.md §13과 동일)

- [ ] `plan`이 무변경 상태에서 0건
- [ ] 신규 유저: `id`가 UPG(gid=uid)+보조그룹 정확, 홈 `0700 user:user`, 셸 nologin, `passwd -S`가 `L`, `pdbedit -L`에 활성
- [ ] SMB로 자기 홈 접근 OK, **다른 홈 `NT_STATUS_ACCESS_DENIED`**, SSH 불가
- [ ] 그룹 폴더 `2770` setgid, 팀원이 만든 파일이 팀 그룹 소유
- [ ] 유저 제거 시 `smbpasswd -d`만·홈 유지 / 재추가 시 재활성
- [ ] uid/gid<1000·범위 밖 → 하드 거부
- [ ] 같은 seed로 `usersync passwd`가 초기 비번 재현
