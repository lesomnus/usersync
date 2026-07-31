# usersync — 설계 노트 (구현 착수용)

> **이 문서 하나로 다른 세션/개발자가 바로 구현할 수 있도록 자립적으로 작성됨.**
> 대상 산출물: `usersync` — 파일 서버의 SMB 사용자/그룹을 **텍스트 파일(선언)** 로 관리하는 **Go 단일 바이너리** reconciler.
> 관련: [`identity-and-sharing.md`](./identity-and-sharing.md)(현재 수동 구성), [`admin-guide.md`](./admin-guide.md)(이 툴이 대체할 수동 절차), [`identity-roadmap.md`](./identity-roadmap.md)(향후 중앙 인증).

---

## 1. 배경 (구현자가 알아야 할 파일 서버 현황)

- **서버**: 파일 서버, Ubuntu 26.04 LTS, ZFS 풀 `tank`. 관리자는 sudo 가능한 계정으로 SSH 접속한다.
- **스토리지 레이아웃** (이미 존재):
  - `tank/research/home` → `/research/home` (개인 홈 루트)
  - `tank/research/groups` → `/research/groups` (그룹 폴더 루트)
  - 둘 다 `sharenfs=off` (개방 NFS와 분리, **SMB 전용**)
- **Samba 4.23** 설치됨. `smb.conf`에 `# >>> hday-shares >>>` 마커 블록으로 `[homes]` + 그룹 공유(`[team-a]` 등) 정의. `smbd`만 active (`nmbd`는 최신 Ubuntu에서 의도적 skip, 정상).
- **사용자 접근 모델 (핵심)**: 연구원은 **SMB로 자기 디렉터리만** 쓰고 **서버 로그인(SSH/콘솔/SFTP)은 불가**해야 함. 이건 다음 3종 세트로 달성:
  1. 로그인 셸 = `/usr/sbin/nologin`
  2. **유닉스 비밀번호 잠금**(`!`) — SSH 비번 인증 차단 (SSH 키도 없음)
  3. **Samba 비번만** 별도 등록(tdbsam) — SMB 인증은 유닉스 비번과 무관
- **홈 권한**: `/research/home/<user>` = `0700`, 소유 `<user>:<user>`(개인 프라이빗)
- **그룹 폴더**: `/research/groups/<team>` = `2770`(setgid), 소유그룹 `<team>` → 그룹원만, 새 파일 자동 그룹 소유
- **현재 테스트 계정**: `alice`(uid 1001), `bob`(uid 1002), 그룹 `team-a`(gid 1001) — **수동 생성분**. 아래 관리 범위(3000+) 밖이므로 usersync는 이들을 건드리지 않음. **구현 착수 전 정리 권장**: `sudo smbpasswd -x alice; sudo userdel alice; sudo rm -rf /research/home/alice`(bob 동일), `sudo groupdel team-a`.

---

## 2. 구현 목적 / 비목적

**목적**
- 사용자/그룹의 **원하는 상태를 버전관리되는 텍스트 파일**로 선언하고, 실행 시 시스템을 그 상태로 **멱등하게 수렴**시킨다.
- Samba 비번 등록·SMB 전용 접근(nologin+잠금)까지 일괄 처리.
- **안전 우선**: 기본 dry-run, 파괴적 삭제 없음(비활성화까지만), 시스템 계정 불가침.
- **의존성 0**: Python/Ruby/인터프리터·패키지 매니저 없이 **정적 단일 바이너리**(Python 회피가 설계 동기 — Ansible 배제 이유).

**비목적**
- 중앙 인증/디렉터리(FreeIPA/AD/Entra)·SSO·소셜 로그인 → [`identity-roadmap.md`](./identity-roadmap.md) 영역. 이 툴은 **로컬 규모** 전용.
- 일반 IAM·권한 정책 엔진이 아님. 오직 유저·그룹·SMB 계정 reconcile.

---

## 3. 운영 워크플로 (모델이 상정하는 사용 흐름)

> **핵심 정신 모델**: `roster.yaml`이 **유일한 진실(single source of truth)**. 관리자는 시스템을 직접
> 만지지 않고 **선언(roster) 만 편집**하고, `usersync`가 시스템을 그 선언으로 **멱등하게 수렴**시킨다.
> Git으로 버전관리 → 변경 이력·리뷰·롤백이 곧 계정 관리 이력. (GitOps 축소판)
>
> 반복되는 안전 리듬: **편집 → `plan`(무변경 미리보기) → `apply`(수렴)**. 파괴는 절대 자동 아님.
>
> (아래 각 명령의 정의는 §8 CLI, 스키마는 §4 참조.)

### 3.1 최초 도입 (기존 수동 서버 → 선언적 관리로 전환)
파일 서버는 이미 수동으로 계정이 있는 상태(§1). 백지에서 시작하지 않는다.
```
usersync export > roster.yaml     # 현재 상태를 우리 포맷으로 흡수(부트스트랩)
$EDITOR roster.yaml               # 검토·정리(보호·범위 밖 계정은 애초에 안 나옴)
git add roster.yaml && git commit -m "adopt usersync"
usersync plan                     # 무변경(action 0건)이어야 정상 = 흡수 정합성 확인
```
이 시점부터 계정 변경은 전부 roster.yaml 편집으로 한다.

### 3.2 신규 연구원 온보딩
```
# roster.yaml 의 users: 에 추가
#   - name: skim
#     uid: 3001
#     full_name: Sunghyun Kim
#     groups: [team-a]
usersync plan --commands          # 나갈 명령 검토(useradd/usermod/smbpasswd …)
usersync apply                    # 생성: UPG → useradd → 홈 → 보조그룹 → 비번잠금 → SMB등록·활성
```
- 초기 SMB 비번은 seed에서 결정적 파생(§7) → 관리자가 재계산해 사용자에게 전달. 서버 로그인은 애초 불가(nologin+잠금), SMB로 자기 홈만.

### 3.3 팀 신설 / 팀 배정 변경
```
# groups: 에 새 팀 추가, users[].groups 수정
usersync plan
usersync apply                    # 그룹 생성+폴더(2770 setgid), 보조그룹은 '정확한 집합으로 치환'
```
- `usermod -G`는 **치환**임에 유의(§6). 모든 팀 소속을 roster가 관리하므로 안전.
- (Phase 2) 새 팀의 `smb.conf` 공유 섹션 자동 생성(§10).

### 3.4 오프보딩 (퇴사·휴직) — 데이터는 보존, UID는 예약
**항목을 지우지 말고 `status`를 바꾼다** — 지우면 uid 예약이 풀려 재사용 사고 위험(§4 생명주기).
```
# 휴직/일시: 해당 user 에 status: disabled 추가
usersync apply                    # smbpasswd -d → SMB 차단, 홈·계정·uid 유지
# 복귀: status 제거(=active) → apply → smbpasswd -e 재활성(비번·데이터 그대로)

# 완전 은퇴: status: reserved (계정은 안 남기고 uid/name 만 예약)
usersync apply                    # 계정 있으면 비활성, uid 재사용은 영구 차단
```
- 완전 삭제가 꼭 필요할 때만: `usersync purge <user>`(홈 아카이브 후 삭제). purge 후에도 **`status: reserved` 항목을 남겨 uid 재사용을 막는다**(§8).

### 3.5 비밀번호 재설정
- usersync는 **기존 계정 비번을 절대 자동 재설정하지 않는다**(사용자가 바꾼 비번 보존). 그래서 분실 대응은 관리자가 명시적으로:
  - 초기 비번으로 되돌리기: seed에서 initpw 재파생해 전달(§7), 또는
  - 임의 재설정: `smbpasswd <user>` 직접 실행.
- 강제-변경 플래그에 의존하지 않음(§7, 클라이언트 UI 편차).

### 3.6 정기 드리프트 점검 (선택, 자동화)
```
usersync plan                     # 무변경이어야 정상. 차이가 나오면 수동 개입 흔적
usersync export | diff - roster.yaml   # 실제↔선언 델타를 직접 확인
```
- cron/CI에서 주기 실행해 "선언과 실제가 어긋났는지"를 감시(예: 관리 범위 안 계정을 누가 손으로 만듦 → orphan 리포트).
- `--json`으로 기계가독 출력 → 알림/대시보드 연동.

### 3.7 이 흐름을 지탱하는 안전 규칙 (재확인)
- **dry-run 우선**: `plan`이 기본 습관, `apply`는 검토 후.
- **파괴 없음**: `apply`는 비활성까지만. 삭제는 `purge` 명시 명령만.
- **보호 가드**: `system_floor`(기본 1000) 미만 + `protect` 예약 범위는 어떤 경로로도 불가침. manage 범위 밖 선언도 거부(§4 관리/보호 범위).
- **멱등**: 같은 roster로 `apply` 반복 → 2회차부터 action 0건.

---

## 4. 입력 파일 스키마 (YAML)

> **포맷 결정: YAML.** 저장소가 이미 `goccy/go-yaml`에 의존하고 운영 설정(`usersync.yaml`)도 YAML이다.
> TOML은 의존성 추가 + 포맷 이원화만 낳으므로 채택하지 않음. TSV 대비 YAML은 그룹 목록을
> 리스트로 자연스럽게 표현하고, 주석·다국어 full_name·향후 필드 확장(예: 만료일)에 유리.
>
> **스키마 정본 = proto.** 런타임 직렬화는 goccy/go-yaml로 하되, 스키마 자체는
> [`proto/usersync/roster.proto`](./proto/usersync/roster.proto)에 정의하고 **protojson 규칙과 호환**되게 유지한다.
> → 향후 `yaml → json → protojson.Unmarshal` 경로로 무변형 이관 가능. 필드명(snake_case)·정수 폭(uid/gid=uint32) 규칙은 proto 파일 주석 참조.

설정은 두 파일로 분리한다.

- **`usersync.yaml`** — *운영 설정*(툴이 어떻게 도는가): 경로·관리/보호 범위·시드·provider. 아래 스키마 참조, override 플래그는 §8.
- **`roster.yaml`** — *원하는 상태*(무엇으로 수렴하는가): 선언된 사용자/그룹. 자주 편집·버전관리 대상.

### `roster.yaml` — 선언 상태
```yaml
# 공유(팀) 그룹. name 은 유일, gid 는 팀 그룹 범위(기본 7000–7999).
groups:
  - name: team-a
    gid: 7001
    description: Perception team
  - name: team-b
    gid: 7002
    description: Planning team

# 사용자. name 은 유일, uid 는 사용자 범위(기본 3000–6999).
users:
  - name: skim
    uid: 3001
    full_name: Sunghyun Kim
    groups: [team-a]            # 보조 그룹(팀). 비면 홈만.
  - name: jlee
    uid: 3002
    full_name: Jiwon Lee
    groups: [team-a, team-b]
  - name: ychoi
    uid: 3003
    full_name: Yuna Choi        # groups 생략 = 팀 없음(홈만)

  # lifecycle: status 생략 = active. 비활성 사용자도 '지우지 말고' 여기 남긴다
  # → uid 가 계속 예약되어 남에게 재배정되지 않음(재사용 사고 방지).
  - name: park                  # 휴직: SMB off, 홈 보존, 되돌리기 쉬움
    uid: 3004
    full_name: Minjun Park
    groups: [team-b]
    status: disabled
  - name: oldhand               # 완전 은퇴: 계정 없음, uid 3005 는 예약(재사용 차단)
    uid: 3005
    full_name: Retired User
    status: reserved
```
- `users[].full_name` = 표시용 실명. 내부적으로 `/etc/passwd`의 **GECOS** 필드(`useradd -c`)로 매핑되지만, roster에는 역사적 명칭 대신 뜻이 보이는 이름으로 노출. 생략 가능. **콤마·콜론 불가**(GECOS 구분자와 충돌 → 로드 시 거부).
- `users[].groups` = **보조 그룹(팀)** 목록. 생략/빈 리스트면 홈만.
- `users[].status` = 생명주기. 생략=`active`. `active | disabled | reserved`(§4 "생명주기" 참조).
- **1차 그룹은 UPG(User Private Group)**: 툴이 사용자명과 동일한 개인 그룹을 만들고 **GID = UID**로 고정(재현성). 즉 `skim:skim`, gid 3001.
- 비번은 파일에 넣지 않음(§7 seed 파생).
- **파서 규칙**: 알 수 없는 키는 거부(strict decode)해 오타를 조기 검출. `name` 중복, **`uid` 중복**, 보호/관리 범위 위반 `uid/gid`, 미정의 팀 참조(`groups`에 없는 팀명)는 로드 시 검증 실패. **유일성은 active/disabled/reserved 전부에 걸쳐 검사** → reserved 항목이 그 uid/name 을 계속 붙들어 재사용을 막는다.

### `usersync.yaml` — 운영 설정
```yaml
paths:
  home:   /research/home          # 개인 홈 루트
  groups: /research/groups        # 그룹 폴더 루트

# usersync 가 '관리'하는 id 창(window). roster 의 uid/gid 는 반드시 이 안이어야 한다.
manage:
  uid: { min: 3000, max: 6999 }   # 사용자(및 UPG gid=uid)
  gid: { min: 7000, max: 7999 }   # 팀 그룹

# usersync 가 '절대 건드리지 않는' 보호(예약) 범위. manage/roster 보다 우선.
# 하드코딩된 "<1000 불가침"을 일반화한 것 — 얼마든 범위를 추가할 수 있다.
protect:
  system_floor: 1000              # 이 값 미만 uid/gid 는 불가침(시스템 계정)
  uid:                            # 추가 보호 대역(선택). 다른 툴/서비스가 쓰는 예약 대역 등.
    - { min: 5000, max: 5099 }
  gid:
    - { min: 8000, max: 8199 }

# manage·protect 어디에도 안 드는 roster 엔트리 처리:
#   error = 하드 거부(기본), skip = 경고 후 그 엔트리만 제외하고 진행(--skip-out-of-scope).
# (protect/floor 를 선언한 엔트리는 이 값과 무관하게 항상 하드 거부.)
on_out_of_scope: error            # error | skip

seed_file: ./seed.secret          # 또는 env USERSYNC_SEED
provider: auto                    # auto | shadow-utils | busybox | pw
```
> 이 스키마도 향후 `config.proto` 로 정본화할 경우 protojson 호환을 유지한다(`min`/`max`·`system_floor` = uint32, `manage.uid` = 메시지, `protect.uid` = repeated 메시지). 지금은 goccy/go-yaml 로만 파싱.

### 관리 범위 & 보호 범위 (하드 가드)
usersync 가 id 를 다루는 규칙은 세 층위로 정리된다.

| 층위              | 대상 id                                   | 규칙                                                                                                                                         |
| ----------------- | ----------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------- |
| **보호(protect)** | `system_floor` 미만 + `protect.uid/gid`   | **절대 생성/수정/삭제/비활성 안 함.** roster 가 이 id 를 선언하면 **로드 시 하드 거부**. 실재해도 usersync 엔 안 보이는 것으로 취급(orphan 리포트 안 함). |
| **관리(manage)**  | `manage.uid/gid` 창(보호와 겹치는 부분 제외) | usersync 가 생성·수렴하는 **유일한** 영역. roster 의 모든 uid/gid 는 여기 있어야 함.                                                          |
| **범위 밖**       | manage 도 protect 도 아닌 나머지          | roster 가 선언하면 **기본은 거부**. `on_out_of_scope: skip`(또는 `--skip-out-of-scope`)이면 **경고 후 그 엔트리만 제외하고 진행**. 실재 계정은 관심 밖(무시). |

- **절대 안전선**: `system_floor` 의 기본·최소값은 **1000**. 설정으로 더 **높일 수는 있어도**(더 안전) **1000 밑으로 낮출 수 없다** — 코드가 `max(1000, system_floor)` 로 클램프(설정으로도 못 뚫음). 기존 "<1000 무조건 거부"를 **일반화하되 안전 보장은 유지**.
- **보호 > 관리**: 두 범위가 겹치면 보호가 이긴다. 예) `manage.uid 3000–6999` 안에 `protect.uid 5000–5099` 를 두면 그 구멍은 관리에서 제외된다.
- UPG 는 `gid = uid` 이므로 사용자 uid 범위가 곧 UPG gid 범위(팀 gid 범위와 분리 유지).

#### 범위 밖 엔트리 처리 — 하드 거부 vs 경고 후 스킵
roster에 **manage에도 protect에도 안 드는** uid/gid 가 섞이는 상황(예: manage 창을 좁혔거나, 다른 대역 계정을 참고용으로 적어둠)에서 **매번 그 줄을 지워야 apply가 도는 건 번거롭다.** 그래서 처리 방식을 고를 수 있게 한다.

| 모드                        | 동작                                                                                     |
| --------------------------- | ---------------------------------------------------------------------------------------- |
| `error` (기본)              | 범위 밖 엔트리가 하나라도 있으면 **로드 시 하드 거부**(exit≠0). 오타·범위 실수 즉시 포착. |
| `skip` (`--skip-out-of-scope`) | 파싱은 정상, **범위 밖 엔트리마다 경고 로그** 후 **그 엔트리만 reconcile에서 제외**하고 나머지는 정상 진행(exit 0). |

- 설정: `usersync.yaml` 의 `on_out_of_scope: error|skip`(기본 `error`), CLI `--skip-out-of-scope` 로 그 실행만 override.
- **적용 대상은 '범위 밖'뿐.** **protect(보호)·`system_floor` 미만을 선언한 엔트리는 이 플래그와 무관하게 항상 하드 거부** — 시스템 대역을 건드리려는 선언은 실수일 확률이 높아 안전선을 유지한다.
- `skip` 이어도 스킵된 엔트리는 **리포트/`--json` 에 `skipped(out-of-scope)` 로 남겨** 조용히 사라지지 않게 한다.

### 생명주기 (`users[].status`) 와 UID 재사용 방지
> **재사용 위험(설계 동기)**: 유저를 삭제하면 그 uid 는 풀린다. 하지만 그 uid 로 소유되던 파일이
> 파일시스템 곳곳(홈·그룹폴더·ZFS 스냅샷·백업)에 **숫자 uid 그대로** 남는다. 나중에 그 uid 를 다른
> 유저에게 재배정하면 **새 유저가 옛 유저의 파일을 통째로 상속**한다(보안·프라이버시 사고).
> → **삭제하지 말고 '은퇴'시킨다.** roster 에 항목을 남기면 uid 가 예약되어 재배정을 막는다.

| status              | 시스템 계정                     | SMB     | 홈   | 재활성 | uid 예약 | 용도                      |
| ------------------- | ------------------------------- | ------- | ---- | ------ | -------- | ------------------------- |
| `active` (기본)     | 있음                            | 활성    | 유지 | —      | O        | 정상                      |
| `disabled`          | **있음**(잠금)                  | 비활성  | 유지 | 쉬움   | O        | 휴직/일시 오프보딩        |
| `reserved`          | **없음**(관리 안 함)            | 없음    | —    | 재선언 | O        | 완전 은퇴(tombstone)      |

- **disabled**: 계정·홈이 남아있으므로 uid 는 자연히 예약됨. `status: active` 로 되돌리면 즉시 재활성(`smbpasswd -e`). 계정이 없으면 usersync 가 **잠긴 상태로 재생성**(언젠가 복귀 전제).
- **reserved**: 계정을 만들지 않는다(있으면 비활성만, 삭제는 `purge` 만). uid/name 만 **점유용 tombstone**. `disabled` 와 달리 계정 없는 상태를 유지 — "번호는 버리되 남에게 안 준다".
- **예약의 강제**: uid/name 유일성 검증이 세 status 전부를 대상으로 하므로, `reserved`(또는 `disabled`) 항목이 있는 한 그 uid/name 을 **새 유저가 못 가져간다**(로드 시 중복 거부).
- **오프보딩 권장 경로**: 항목 삭제(→ uid 예약 해제, 재사용 위험) 대신 `status: disabled`(복귀 여지) 또는 `status: reserved`(영구). 완전 삭제가 꼭 필요하면 §8 `purge`, 그리고 **freed uid 를 reserved 로 남겨** 재사용을 막는다.
- 그룹 gid 재사용도 같은 위험이 있다 — 현재는 group 항목을 roster 에 남겨 gid 를 예약(향후 group `status` 로 대칭 확장 가능).

---

## 5. Reconcile 상태표 (desired × actual → action)

"actual"은 시스템에서 수집: `getent passwd/group`(범위 필터) + `pdbedit -L -v`(SMB 계정·플래그).

### 그룹
| desired | actual               | action                                              |
| ------- | -------------------- | --------------------------------------------------- |
| 있음    | 없음                 | `groupadd -g <gid> <name>` + 그룹 폴더 생성(§6)     |
| 있음    | 있음, gid 일치       | 폴더 퍼미션 보정(2770·소유그룹) — 맞으면 no-op      |
| 있음    | 있음, **gid 불일치** | **거부·리포트**(gid 변경은 위험, 수동)              |
| 없음    | 있음(관리 범위)      | **orphan 리포트**, 삭제 안 함 (purge는 명시 명령만) |

### 사용자
desired 는 `status`(생략=`active`) 기준. `uid 불일치`(같은 name, 다른 uid)는 모든 status에서 **거부·리포트**(uid 변경 = chown 지옥, 수동).

| desired status | actual                       | action                                                                                                                        |
| -------------- | ---------------------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| `active`       | 없음                         | **생성**: UPG(`groupadd -g <uid> <user>`) → `useradd` → 홈 생성 → 보조그룹 설정 → 유닉스 비번 잠금 → SMB 비번(seed) 등록·활성 |
| `active`       | 있음, SMB 활성               | 보조그룹·홈 퍼미션 보정. **비번 절대 재설정 안 함**                                                                           |
| `active`       | 있음, SMB **비활성**(재등장) | `smbpasswd -e <user>` 재활성                                                                                                  |
| `disabled`     | 없음                         | **잠금 상태로 생성**(계정·UPG·홈·보조그룹까지, SMB는 등록하되 `-d`로 비활성). 복귀 전제                                       |
| `disabled`     | 있음, SMB 활성               | `smbpasswd -d <user>` 비활성. 홈·계정 **보존**                                                                                |
| `disabled`     | 있음, 이미 SMB 비활성        | no-op (멱등)                                                                                                                  |
| `reserved`     | 없음                         | **no-op — uid/name 예약만**(계정 생성 안 함)                                                                                  |
| `reserved`     | 있음                         | 활성이면 `smbpasswd -d`, **리포트**(`reserved: 계정 잔존, 완전 제거는 purge`). **재생성·삭제 안 함**                          |
| (roster에서 빠짐) | 있음(관리 범위)           | **(b) 자동 비활성화**: `smbpasswd -d <user>`, 홈 **보존**, orphan 리포트. 삭제 안 함. ⚠︎ **삭제는 uid 예약을 푸므로**, 은퇴는 `disabled`/`reserved` 권장(§4 생명주기) |

> **(b) 자동 비활성화 채택.** 파일에서 빠지면 SMB 로그인만 차단, 데이터 유지. 완전 삭제는 §8 `purge`로만.
> 단, 항목을 지우면 **uid 예약이 풀려 재사용 위험**(§4)이 생기니, 실무에선 지우지 말고 `status: disabled|reserved` 로 남기는 걸 권장.

---

## 6. Provider가 수행하는 동작 — shadow-utils 구현 기준

> 아래 명령은 **`Provider`/`Samba` 인터페이스(§9)의 shadow-utils 구현**이 내부적으로 exec하는 것.
> 리컨사일러는 이 명령들을 직접 알지 못하고 인터페이스 메서드만 호출한다(busybox·pw·mock으로 교체 가능).
> `plan`은 계획된 action을 출력만 하고, `apply`만 provider 메서드를 실제 호출한다(별도 dry-run provider 불필요).

```bash
# --- 그룹 생성 ---
groupadd -g <gid> <name>
mkdir -p /research/groups/<name>
chgrp <name> /research/groups/<name>
chmod 2770 /research/groups/<name>          # drwxrws--- (setgid)

# --- 사용자 생성 ---
groupadd -g <uid> <user>                    # UPG, gid=uid
useradd -u <uid> -g <uid> -M \
        -d /research/home/<user> \
        -s /usr/sbin/nologin \
        -c "<full_name>" <user>              # roster.full_name → GECOS 필드
usermod -G <team1,team2> <user>             # 보조그룹 = 파일 기준 '치환'(주의: -a 아님)
mkdir -p /research/home/<user>
chown <user>:<user> /research/home/<user>
chmod 0700 /research/home/<user>
usermod -L <user>                           # 유닉스 비번 잠금(SSH 차단). useradd가 이미 '!'지만 명시
printf '%s\n%s\n' "<initpw>" "<initpw>" | smbpasswd -a -s <user>
smbpasswd -e <user>

# --- 보조그룹 보정(기존 사용자) ---
usermod -G <team1,team2> <user>             # 파일이 진실 → 정확한 집합으로 치환

# --- 비활성(b) / 재활성 ---
smbpasswd -d <user>
smbpasswd -e <user>
```
주의:
- `usermod -G`는 보조그룹을 **치환**한다. 연구원은 전부 이 툴이 관리하므로 OK, 단 문서화 필수.
- 홈 소유 UID/GID는 UPG(gid=uid) 기준. 파일 내부 소유권은 setgid 그룹 폴더에서만 팀 그룹으로.

---

## 7. 비밀번호 정책 (seed 파생, 생성 시 1회)

- **seed는 파일에 없음.** mode `0600` 파일(`seed.secret`, **gitignore**) 또는 env `USERSYNC_SEED`로 주입.
- **초기 비번 = 결정적 파생**(관리자가 재계산해 사용자에게 전달 가능):
  ```
  initpw(user) = "Hd-" + base32( HMAC_SHA256(seed, "usersync:v1:"+username) )[:12]
  ```
  (base32 대문자, 혼동문자 배제. 접두 `Hd-`로 복잡도 최소보장. Go: `crypto/hmac`+`crypto/sha256`+`encoding/base32`, 전부 stdlib.)
- **생성 시(SMB 계정 부재)만 설정.** 기존 계정엔 절대 재설정 안 함 → 사용자가 바꾼 비번 보존.
- **변경 강제**: 비-AD 표준 Samba에선 "다음 로그인 시 변경 강제"가 **Windows에서만** 매끄럽고 mac/Linux엔 UI 없음 → **강제 대신 안내**. 서버 셸을 막으므로, 못 바꾸는 클라이언트는 관리자 재설정으로 대응. (must-change 플래그 의존하지 말 것.)

---

## 8. CLI 인터페이스

```
usersync plan     # 기본. dry-run: 수집→diff→action 목록만 출력, 무변경. exit 0
usersync apply    # 실제 실행(멱등). 요약 리포트 출력
usersync export   # 현재 시스템 상태(관리 범위)를 roster.yaml 포맷으로 역출력(stdout)
usersync purge <user>   # 명시적 완전 삭제(위험): 홈 tar 아카이브 → smbpasswd -x → userdel → groupdel(UPG)
```
> `purge` 후에는 그 uid/name 을 `status: reserved` 로 roster 에 남기는 것을 권장(재사용 차단, §4 생명주기).
> `--reserve`(기본 on)면 purge 가 roster 에 reserved tombstone 항목을 자동 추가; `--no-reserve` 로 끌 수 있다.

### `export` — 실제 상태 → 우리 포맷 (역방향)
- 시스템의 실제 사용자/그룹을 **관리 범위 안에서** `Scan`(§9, `getent`+`pdbedit`)으로 수집해 `roster.yaml`을 **stdout으로** 출력. `> roster.yaml`로 저장.
- 용도: 이미 수동 구성된 서버에서 roster **부트스트랩**(Terraform `import` 격), 또는 실제↔선언 드리프트 확인.
- **정합성 보증**: 방금 export한 roster를 그대로 `plan`에 넣으면 **action 0건**이어야 한다(export/scan/reconcile 왕복 일치 = 회귀 테스트 지점).
- **권한**: 유저/그룹(`getent`)은 root 불필요. SMB 상태(`pdbedit`)만 root 필요 → 비-root면 SMB 활성/비활성 정보는 생략하고 경고(유저/그룹은 정상 출력).
- 결정적 출력: name 기준 정렬해 diff·버전관리에 안정적.

### `plan --commands` — 나갈 명령 미리보기
- 기본 `plan`은 상위 action 목록(아래 리포트 예). `--commands`를 주면 각 action이 실제로 실행할 **백엔드 명령 원문**(§6, `useradd -u 3001 …`, `smbpasswd -a …`)을 순서대로 함께 출력.
- provider별로 달라지는 실제 명령을 실행 전에 검토·감사하거나, 스크립트로 뽑아둘 때 사용. **여전히 무변경**(dry-run).
- 표준 루프:
  ```
  usersync export > roster.yaml   # 현재 상태를 우리 포맷으로
  $EDITOR roster.yaml             # 편집
  usersync plan --commands        # 나갈 명령 검토 (실행 안 함)
  usersync apply                  # 실행
  ```
> 운영 설정(경로·manage/protect 범위·시드·provider)은 `usersync.yaml`에 둔다(§4). 자주 안 바뀌고 안전에 직결되는 값이라 플래그가 아닌 **리뷰되는 설정 파일**이 정본. 경로류만 아래 플래그로 개별 override.
> roster(선언 상태)는 `roster.yaml`(`--roster`)에 둔다. 둘의 역할 분리는 §4 참조.

공통 플래그:
- `--roster roster.yaml` (기본 `roster.yaml`)
- `--config usersync.yaml` (운영 설정: 경로·manage·protect·시드·provider; 기존 root flag 재사용)
- `--home-base /research/home --groups-base /research/groups` (config `paths` override)
- `--seed-file ./seed.secret` (또는 env `USERSYNC_SEED`)
- `--json` (리포트를 기계가독 JSON으로)
- `--commands` (`plan` 전용: 실행될 백엔드 명령 원문까지 출력)
- `--skip-out-of-scope` (범위 밖 roster 엔트리를 하드 거부 대신 경고 후 스킵; config `on_out_of_scope: skip` override)
- `--yes` (purge 확인 스킵)
> manage/protect 범위는 리스트·안전 직결값이라 플래그로 노출하지 않고 `usersync.yaml`에서만 설정한다.

동작 규칙:
- **root 요구는 명령별**: `apply`/`purge`는 **root 필수**(euid==0 아니면 즉시 에러). `plan`은 root 불필요(무변경). `export`는 유저/그룹만이면 불필요, SMB 상태까지 원하면 root(없으면 경고 후 SMB 생략).
- `apply`는 **삭제/파괴 없음**(비활성화까지만). 삭제는 `purge`만.
- **보호 범위(`system_floor` 미만·`protect`)를 선언·조작하면 항상 즉시 중단·에러**(하드 가드, §4). **관리 범위 밖** 엔트리는 기본 거부지만 `on_out_of_scope: skip`(`--skip-out-of-scope`)이면 경고 후 스킵하고 진행(§4).
- **멱등**: 파일 무변경 상태로 `apply` 재실행 → action 0건.
- 종료 코드: 성공 0, 거부(gid/uid 불일치 등 수동개입 필요) 비0.

리포트 예:
```
PLAN (dry-run)
  + create user  skim (uid 3001, groups: team-a)
  + create group team-b (gid 7002)
  ~ update user  jlee (groups: team-a → team-a,team-b)
  - disable user oldie (absent in file; home kept)
  ! refuse group team-a (gid 7001 desired, 5000 actual — manual)
  · skip   user  legacy (uid 2000 out of manage scope; --skip-out-of-scope)
Summary: create=1 update=1 disable=1 refuse=1 group-create=1 skip=1
```

---

## 9. Provider / Samba 인터페이스 (백엔드 추상화)

> **설계 동기**: `useradd`/`usermod`은 shadow-utils 전용이다. busybox(Alpine)는 `adduser`/`addgroup`을
> 다른 플래그로 쓰고 **`usermod`이 없어** 보조그룹은 `addgroup <user> <group>`으로 하나씩 붙인다.
> BSD 계열은 `pw useradd`. 계정 조작 명령을 리컨사일러에 하드코딩하면 이식성이 죽고 테스트에 root가 필요해진다.
> → **계정 조작을 인터페이스 뒤로 숨기고, 백엔드별 구현 + 테스트용 fake를 주입**한다.

관심사를 둘로 나눈다(직교 — SMB 없이 OS 계정만, 혹은 다른 SMB 스택도 가능하도록):

```go
// UserSpec/GroupSpec 은 roster + 계산된 값(UPG gid=uid 등)으로 리컨사일러가 채워 넘긴다.
type GroupSpec struct { Name string; GID int }
type UserSpec  struct { Name string; UID int; FullName, Home, Shell string } // FullName → GECOS

// Provider: OS 사용자/그룹 생명주기. 백엔드 1개당 구현 1개.
//   shadowutils(useradd/usermod/groupadd) | busybox(adduser/addgroup) | bsd(pw) | fake(test)
type Provider interface {
	// 관리 범위(uid/gid) 안의 실제 상태를 수집. getent 파싱 등.
	Scan(ctx context.Context) (State, error)

	EnsureGroup(ctx context.Context, g GroupSpec) error
	EnsureUser(ctx context.Context, u UserSpec) error
	// 보조그룹을 '정확한 집합으로 치환'. 없는 팀은 backend 내부에서 add/remove 로 흡수.
	SetSupplementaryGroups(ctx context.Context, user string, groups []string) error
	// 유닉스 비번 잠금(SSH 차단). shadow: `usermod -L`, busybox: `passwd -l`, bsd: `pw lock`.
	LockPassword(ctx context.Context, user string) error
}

// Samba: SMB(tdbsam) 자격증명 집합. OS provider 와 직교.
//   smbpasswd/pdbedit 래핑. 향후 다른 SMB 백엔드로 교체 가능.
type Samba interface {
	Accounts(ctx context.Context) (map[string]SmbAccount, error) // pdbedit -L -v
	Create(ctx context.Context, user, initialPassword string) error
	Enable(ctx context.Context, user string) error
	Disable(ctx context.Context, user string) error
	Delete(ctx context.Context, user string) error // purge 전용
}
```

- **리컨사일러는 순수함수**: `Reconcile(desired Roster, actual State) []Action`. 인터페이스도 명령도 모름 → root 없이 단위테스트.
- **apply 루프가 dispatcher**: 각 `Action`을 `Provider`/`Samba` 메서드로 매핑해 실행. `plan`은 action을 출력만.
- **백엔드 선택**: `usersync.yaml`의 `provider: auto|shadow-utils|busybox|pw`. `auto`는 PATH에서 `useradd`→`adduser`→`pw` 순으로 탐지. MVP는 **shadow-utils만** 구현하고 나머지는 인터페이스만 남겨 stub.
- **`os/exec` 래퍼**: 각 backend 구현은 명령 실행을 얇은 `runner`(주입 가능한 `func(ctx, name, args...) error`)로 감싸 golden-command 테스트 가능하게 한다.

---

## 10. (선택, Phase 2) smb.conf 그룹 공유 자동 생성

새 팀 그룹을 추가하면 `smb.conf`에 해당 `[team-x]` 공유 섹션이 있어야 SMB에 노출됨. 옵션 기능:
- `smb.conf`의 `# >>> usersync-shares >>>` … `# <<< usersync-shares <<<` 마커 블록을 **roster.yaml의 groups에서 재생성**(각 팀마다 아래 템플릿), `[homes]`는 1회 고정 삽입.
- 변경 시 `testparm`로 검증 후 `systemctl reload smbd`.
- 실패 시 원본 복원(`smb.conf.bak`).
```ini
[<team>]
   comment = <team> shared
   path = /research/groups/<team>
   browseable = yes
   read only = no
   valid users = @<team>
   force group = <team>
   create mask = 0660
   directory mask = 2770
```
> MVP에선 제외하고 수동 유지 가능(§admin-guide). 자주 팀이 바뀌면 Phase 2로 자동화.

---

## 11. 기술 스택 / 구조

- **Go 1.26**, 현 저장소 스캐폴딩 재사용: CLI = `lesomnus/xli`, 설정 = `goccy/go-yaml`, 관측 = `otx`/`mkot`, 헬퍼 = `lesomnus/z`. `greet`/`config` 예시 명령이 그대로 참고 템플릿.
- **정적 바이너리**: 계정 조작은 CGO 없이 명령 exec로 하므로 `CGO_ENABLED=0 go build` 유지 가능. (yaml/otx 의존은 순수 Go.)
- **리눅스 계정 생성 네이티브 API는 없음** → §6 명령을 exec하는 **오케스트레이터**. `/etc/passwd` 직접 쓰기 금지(락킹·PAM 위험). 모든 exec는 §9 `Provider`/`Samba` 인터페이스 뒤에.
- 제안 패키지 구조 (현 `cmd/` 레이아웃에 통합):
  ```
  cmd/
    root.go              # 기존. plan/apply/export/purge 서브커맨드 등록
    plan.go  apply.go  export.go  purge.go
  cmd/config/
    config.go            # 기존 Config 확장: Paths/Manage/Protect/Seed/Provider(운영설정)
    roster.go            # roster.yaml 파싱·검증 → Roster{Users,Groups}
  internal/
    idrange/             # Manage/Protect 범위 판정(포함·보호 우선·floor 클램프) + 테스트
    reconcile/           # 순수함수 Reconcile(Roster, State) []Action  (핵심 단위테스트)
    provider/            # Provider 인터페이스 + shadowutils/ busybox(stub)/ fake(test)
    samba/               # Samba 인터페이스 + smbpasswd 구현 + fake
    secret/              # seed 파생(HMAC → base32)
    report/              # 텍스트/JSON 리포트
  roster.yaml            # 예시 roster (users/groups)
  # seed.secret 는 .gitignore
  ```
- **테스트 용이성**: `reconcile`·`idrange`는 순수함수 → root 없이 단위테스트. `Provider`/`Samba`는 fake 주입, 실제 구현은 injectable `runner`로 golden-command 테스트.

---

## 12. 구현 계획 (단계별)

> 각 단계 끝에 실행 가능한 산출물이 남도록 세로로 얇게 자름. `reconcile`·`idrange`·`secret`은 root 없이 CI에서 테스트.

### 진행 상황 (2026-07-31)
- ✅ **순수 코어**: `internal/idrange`(분류+floor 클램프), `internal/secret`(seed 파생+golden), `internal/roster`(types+strict load+validate), `internal/state`, `internal/reconcile`(status×actual 매트릭스). 단위테스트 통과.
- ✅ **백엔드**: `internal/run`(injectable exec+Fake), `internal/provider`(shadow-utils: getent Scan + useradd/usermod/groupadd, golden-command 테스트), `internal/samba`(smbpasswd/pdbedit), `internal/fsops`(홈/그룹 폴더), `internal/report`(text/JSON), `internal/executor`(dispatch+Collect, fake 테스트).
- ✅ **CLI**: `cmd/config` 운영설정(paths/manage/protect/on_out_of_scope/seed/provider) + `plan`(`--commands` 미리보기)·`apply`·`export`·`purge`(`--reserve` tombstone). greet 스캐폴딩 제거.
- ✅ **기능 검증**: 스텁 getent/pdbedit로 E2E — plan/commands/export 동작, **`export | plan` = 0 action**(멱등 왕복), out-of-scope error·skip, protected 하드 거부 확인.
- 🚧 **남음**: `apply` 실계정 통합검증(root 필요, 파일 서버), busybox/pw provider, smb.conf 자동생성(Phase 2), 적대적 리뷰.

**0단계 — 스캐폴딩 정리**
- `greet` 예시 명령/설정 제거. `cmd/config.Config`에 운영 필드 추가: `paths`(home/groups), `manage`(uid/gid 창), `protect`(system_floor + uid/gid 예약 범위), `on_out_of_scope`(error|skip), `seed_file`, `provider`. §8 플래그와 매핑(`z.FallbackP` 기본값: uid 3000–6999, gid 7000–7999, system_floor 1000, on_out_of_scope=error, provider auto).
- `idrange` 패키지: id → `Protected|Managed|OutOfScope` 분류. **`system_floor`는 `max(1000, cfg)`로 클램프**(1000 밑 불가). "보호 > 관리 > 범위 밖" 판정 함수 + 표 케이스 단위테스트.
- `roster.yaml` 로더(`cmd/config/roster.go`): strict decode + 검증. **분류별 처리**: `Protected` 선언 → 항상 하드 에러; `OutOfScope` 선언 → `on_out_of_scope`에 따라 하드 에러 또는 경고 후 그 엔트리 드롭(스킵 목록 보존); 그 외 미정의 팀 참조 검증. **유일성**: `name`·`uid` 중복을 **active/disabled/reserved 전체에 걸쳐** 거부(= 예약 강제).

**1단계 — reconcile 코어 (root 불필요, 순수 로직)**
- 타입: `Roster{Users,Groups}`, `User.Status`(active|disabled|reserved), `State`(actual), `Action`(create/create-locked/update/enable/disable/refuse/skip/reserve-noop…).
- `Reconcile(Roster, State) []Action` 를 §5 상태표대로 구현(**status × actual** 매트릭스 포함) + **하드 가드**(idrange: 보호/floor 즉시 거부, 범위 밖은 정책에 따라 거부 또는 skip action).
- `report`(텍스트) + `plan` 서브커맨드: State 를 fake로 주입해 계획 출력. **단위테스트 최우선**(멱등·거부·disabled/reserved·재사용거부 케이스).

**2단계 — provider/samba 인터페이스 + shadow-utils 구현**
- `Provider`(§9) shadowutils 구현: `Scan`(getent 파싱, 보호/범위 필터), Ensure/Set/Lock. injectable `runner`로 golden-command 테스트.
- `Samba` 구현: `pdbedit` 파싱 + `smbpasswd -a/-e/-d`. `secret` 패키지로 initpw 파생(§7).
- busybox/pw 는 인터페이스 stub만(미구현 시 명확한 에러).
- **`export` 서브커맨드**: `Scan` → `State`를 `Roster`로 변환 → roster.yaml 인코딩(stdout, name 정렬). 비-root면 SMB 생략+경고. **왕복 테스트**: `export | plan` = action 0건.

**3단계 — apply + 안전장치**
- `apply` 서브커맨드: root(euid==0) 체크 → `Scan` → `Reconcile` → action dispatch. 멱등 확인.
- **`plan --commands`**: dispatch를 **print-only runner**로 돌려 실행 대신 백엔드 명령 원문을 출력(무변경). apply와 같은 코드 경로 → 미리보기와 실제 실행 일치 보장.
- `--json` 리포트, 종료코드 정리(거부 시 비0), `--yes`.

**4단계 — Phase 2 (선택)**
- `purge <user>`(홈 tar 아카이브 → Samba.Delete → user/UPG 삭제).
- `smb.conf` 마커 블록 자동 생성(§10) + `testparm`/reload.
- `admin-guide.md`를 "roster.yaml 편집 → `usersync apply`"로 갱신.

---

## 13. 검증 체크리스트 (완료 기준)

- [ ] `plan`이 무변경 상태에서 action 0건 (멱등)
- [ ] 신규 유저 생성 후: `id`가 UPG(gid=uid)+보조그룹 정확, 홈 `0700 user:user`, 셸 nologin, `passwd -S`가 `L`(잠김), `pdbedit -L`에 존재·활성
- [ ] 신규 유저가 SMB로 자기 홈 접근 OK, **다른 유저 홈 `NT_STATUS_ACCESS_DENIED`**, **SSH 로그인 불가**(비번 잠김·nologin)
- [ ] 그룹 폴더 `2770` setgid, 그룹원이 만든 파일이 팀 그룹 소유
- [ ] 파일에서 유저 제거 → `apply` 시 `smbpasswd -d`만, 홈 데이터 유지 / 다시 추가 → 재활성
- [ ] `status: disabled` → SMB 비활성·홈/계정/uid 유지, `active` 로 되돌리면 재활성 (멱등)
- [ ] `status: reserved` → 계정 안 만듦(있으면 비활성), uid/name 점유 유지
- [ ] **uid 재사용 차단**: reserved(또는 disabled) 항목의 uid/name 을 다른 유저가 선언 → 로드 시 중복 거부
- [ ] 보호 범위(`system_floor` 미만·`protect` 예약 대역) 선언 엔트리 → 플래그와 무관하게 항상 하드 거부
- [ ] 관리 범위 밖 엔트리 → 기본(`error`) 하드 거부 / `--skip-out-of-scope`(=`on_out_of_scope: skip`) 시 경고 후 그 엔트리만 스킵(리포트에 `skip` 표기)하고 나머지 정상 진행(exit 0)
- [ ] `system_floor`를 1000 밑으로 설정해도 코드가 1000으로 클램프(설정으로 못 뚫음)
- [ ] `protect.uid/gid`에 넣은 대역은 실재 계정이 있어도 무시(orphan 리포트·비활성 안 함)
- [ ] seed 동일 시 initpw 재현(관리자 재계산 가능)
