# identity-roadmap — 자격증명과 온프렘 AD 전환

> 상태: 결정됨(자격증명) / 계획(AD 전환). 최종 갱신 2026-08-07
> 관련: [`plan.md`](./plan.md)(usersync 설계), [`admin-guide.md`](./admin-guide.md)(운영 절차)

이 문서는 두 가지를 다룬다.

1. **지금** 누가 사용자를 인증하는가 — 결정 완료, 아래 §1
2. **언젠가** 온프렘 AD가 들어올 때 무엇이 어떻게 바뀌는가 — 계획, §3 이후

---

## 1. 자격증명 아키텍처 (현재 결정)

**비밀번호는 하나다. Samba의 `tdbsam`에 있고, 웹과 SMB가 같은 것을 쓴다.**

| 경로 | 인증 방법 |
| ---- | --------- |
| SMB (탐색기 / Finder) | Samba가 tdbsam으로 직접 인증 |
| 웹 UI | 서버가 `ntlm_auth`를 호출해 **같은 tdbsam**에 검증을 위임 |

usersync는 계정을 만들 때 시드 파생 초기 비밀번호를 `smbpasswd`로 등록하고(§[plan.md 7](./plan.md)), 그 뒤로는 절대 덮어쓰지 않는다. 사용자가 바꾼 비밀번호는 두 경로 모두에서 그대로 통한다.

### 왜 SSO(OIDC)를 쓰지 않는가

검토했고, 지금 조건에서는 채택하지 않기로 했다.

- **SMB는 어차피 SSO를 못 쓴다.** SMB2/3의 인증은 SPNEGO로 감싼 Kerberos 또는 NTLMSSP뿐이고, OIDC 토큰이 들어갈 자리가 없다. KDC가 없는 지금 SMB는 무조건 자기 자격증명을 쓴다. **SMB의 SSO는 Kerberos가 유일한 답이며, 그건 AD가 와야 생긴다.**
- **IdP 커버리지가 전 직원이 아니다.** 사내 Entra는 가장 낮은 등급이고 사실상 이메일 전용이라, 계정이 없는 인원이 있다. 그들은 OIDC로 로그인할 수단이 없다.
- **그래서 SSO는 도움이 가장 덜 필요한 층을 돕는다.** 이메일을 매일 쓰는 사람에게는 편하지만, 이 시스템이 명시적으로 우선한 "컴퓨터에 익숙하지 않은 사용자"는 대체로 그 반대편에 있다.
- **웹만 SSO를 붙이면 자격증명이 둘이 된다.** 그러면 오프보딩도 두 곳에서 해야 하고, IdP만 잠그면 **웹은 막히는데 SMB 비밀번호는 살아 있는** 상태가 조용히 남는다.

단일 저장소는 그 반대다. `roster.yaml`에서 `status: disabled` 한 줄이면 웹과 SMB가 **동시에** 죽는다.

### 대가: MFA가 없다

이 구성에서 실질적으로 포기하는 것은 MFA 하나다. **수용 조건을 명시한다: 서버를 인터넷에 직접 노출하지 않고 사내망/VPN 한정으로 운영한다.** 이 전제가 깨지면 이 결정을 다시 열어야 한다.

### 결정을 뒤집는 조건

- 외부 노출이 필요해짐
- 온프렘 AD 도입으로 전 직원이 도메인 계정을 갖게 됨 (→ §3 이후)
- Entra 커버리지가 전원으로 확대됨

그래서 **웹 인증은 인터페이스 뒤에 둔다.** 구현은 `ntlm_auth` 하나지만, 교체 지점을 미리 만들어 둔다.

### 운영 전제: winbindd

`ntlm_auth`는 winbindd를 경유한다. standalone Samba에서는 winbindd를 띄우지 않는 경우가 많으므로, **웹 인증을 붙이기 전에 winbindd를 실행 상태로 만들어야 한다.** 어차피 AD 조인 후에는 필수 구성요소이므로, 지금 켜 두면 전환 시 바뀔 것이 하나 줄어든다.

> 정확한 호출 형태(`ntlm_auth --username=... --password=...` vs `--helper-protocol=ntlm-server-1`)와 권한 요건(root 또는 `winbindd_priv` 그룹)은 배포 대상 Samba 버전에서 실측할 것.

---

## 2. AD가 오면 무엇이 해결되는가

| 지금 | AD 이후 |
| ---- | ------- |
| 비밀번호 1개(파일서버 전용, 새로 외워야 함) | 도메인 비밀번호 1개(이미 쓰는 것) |
| SMB 마운트 시 비밀번호 입력 | 관리 단말은 **Kerberos 무입력** |
| MFA 없음 | AD 연계 IdP로 웹 MFA 가능 |
| 웹 = `ntlm_auth` → tdbsam | 웹 = `ntlm_auth` → winbind → AD. **코드 변경 0** |

마지막 줄이 §1에서 tdbsam 단일화를 고른 실질적 근거다. 전환일에 웹 인증 코드는 손대지 않는다.

**단, Kerberos 무입력은 단말이 관리 대상일 때만이다.** 도메인 조인한 Windows는 자동, MDM으로 Kerberos SSO Extension을 배포한 macOS도 자동, Linux는 `kinit` 필요. 미관리 단말(BYOD)은 여전히 비밀번호를 입력한다 — 다만 그때는 **이미 아는 도메인 비밀번호**다.

---

## 3. 전환의 본질: uid 번호의 소유권 이동

데이터는 움직이지 않는다. 바뀌는 것은 **"skim은 3001번"이라고 답하는 주체**가 `/etc/passwd`에서 AD로 넘어가는 것 하나다.

### 절대 원칙: uid 번호는 바뀌면 안 된다

라이브 파일은 `chown -R`로 고칠 수 있다. **ZFS 스냅샷은 불변이라 고칠 수 없다.**

재번호를 하는 순간 모든 과거 스냅샷이 영구히 "틀린 uid"로 남고, 그 번호를 나중에 누가 받으면 스냅샷 복원 시 남의 파일을 상속한다 — usersync의 tombstone이 애초에 막으려던 바로 그 사고를, 마이그레이션이 스스로 만들어내는 꼴이다.

### 해법: `idmap_ad` + RFC2307

winbind의 id 백엔드를 `ad`로 두면 uid가 **AD 객체의 `uidNumber` 속성에서 온다.** 그 값을 우리가 정할 수 있으므로, roster의 번호를 그대로 써넣으면 된다.

```sh
usersync export --format csv > ids.csv
```
```powershell
Import-Csv ids.csv | Where-Object type -eq 'group' |
  ForEach-Object { Set-ADGroup $_.name -Replace @{gidNumber=[int]$_.gid_number} }
Import-Csv ids.csv | Where-Object type -eq 'user'  |
  ForEach-Object { Set-ADUser  $_.name -Replace @{uidNumber=[int]$_.uid_number; gidNumber=[int]$_.gid_number} }
```

결과: **chown 0회, 데이터 이동 0회, 권한 변경 0회.**

---

## 4. 지금 확보해 둘 것

### ID 대역 예약 — IT에 문서로 남길 것

> **uid/gid `3000`–`19999`는 파일서버가 이미 사용 중이다. AD 도입 시 이 대역의 `uidNumber`/`gidNumber`를 다른 용도로 할당하지 말 것.**

내역은 `usersync.yaml`의 `manage` 창이 정본이다.

| 대역 | 용도 |
| ---- | ---- |
| `3000`–`9999` | 사용자 uid. UPG의 gid가 uid와 같으므로 이 번호대는 gid로도 소비된다 |
| `10000`–`19999` | 팀 그룹 gid |

**예약은 일회성 협상이고, 번호는 공짜다.** 현 인원보다 넉넉하게 잡아 둔 이유가 이것이다 — 나중에 넓히려면 다시 협상해야 하고, 모자라서 재번호하는 건 위 원칙상 불가능하다.

`reserved` tombstone이 붙든 번호도 이 예약에 포함된다. AD에는 그 계정이 없으므로 속성으로 표현되지 않고, **대역 전체를 막는 것으로만 보호된다.** 그래서 roster는 전환 후에도 폐기되지 않고 장부로 남는다.

### 이름 규칙

`sAMAccountName`이 로컬 username과 **정확히 일치**해야 조인 키가 성립한다. AD 도입 전에 명명 규칙을 확정하고 그대로 쓸 것. 소문자, 20자 이하.

이름은 나중에 바꿀 수 있다(파일은 번호 소유라 무사하다). 대신 홈 디렉터리 이동과 공유 설정 수정이 따라온다. **번호를 바꾸는 것보다는 훨씬 싸다.**

---

## 5. ⚠️ 전제 확인: Entra ID로는 안 된다

**Samba는 Entra ID(클라우드)에 도메인 조인할 수 없다.** Entra는 Samba가 요구하는 LDAP·Kerberos를 전통적 방식으로 제공하지 않는다. `net ads join`이 되는 대상은 진짜 AD DS뿐이다.

| 갈래 | Kerberos SSO | uid 제어 | 판단 |
| ---- | ------------ | -------- | ---- |
| **온프렘 AD DS 구축** | ✅ | ✅ `uidNumber` 직접 지정 | **이 문서가 전제하는 경로** |
| Entra Domain Services | ✅ | ⚠️ 클라우드 전용 사용자는 POSIX 속성 자동 생성이라 지정이 어려움 → `idmap_nss`로 로컬 passwd를 권위로 유지해야 함 | Azure 구독 비용 별도. 현 프로필에서 가능성 낮음 |
| Entra만 계속 사용 | ❌ **영원히 없음** | — | tdbsam + usersync가 **영구 구조**가 된다 |

**확인할 것:** 도입 대상이 온프렘 AD DS인가, 그리고 M365 계정과 연결하려면 **Entra Connect 동기화까지 세트로** 계획해야 한다(동기화하지 않으면 이메일 계정과 도메인 계정이 별개가 되어 신원이 오히려 둘로 갈라진다).

세 갈래 중 무엇이든 **§4의 준비는 전부 유효하다** — 대역 예약, tombstone, 명명 규칙은 갈래와 무관하다.

---

## 6. `smb.conf` 변경

```ini
# --- 지금 (standalone) ---
   security = user
   passdb backend = tdbsam

# --- 전환 후 (domain member) ---
   security = ADS
   realm = CORP.EXAMPLE.COM
   workgroup = CORP

   winbind use default domain = yes      # @team-a 표기 유지 → shares 블록 무변경
   template shell = /usr/sbin/nologin    # 기본값도 /bin/false 지만 의도를 드러내 둔다
   template homedir = /research/home/%U  # ★ 필수. 기본값이 /home/%D/%U 라 빠뜨리면 홈이 어긋난다

   idmap config * : backend = tdb
   idmap config * : range = 100000-199999
   idmap config CORP : backend = ad
   idmap config CORP : schema_mode = rfc2307
   idmap config CORP : range = 3000-19999     # ← §4의 대역 그대로
   idmap config CORP : unix_primary_group = yes
```

- `idmap config CORP : range`는 **필터로도 동작**한다. 대역 전체(3000–19999)를 잡아야 UPG gid까지 해석된다.
- `winbind use default domain = yes` 덕분에 `valid users = @team-a`가 그대로 유효하다 → **`usersync shares`가 생성하는 블록은 변경 불필요.**
- **둘 중 진짜 위험한 쪽은 `template homedir` 이다.** 기본값이 `/home/%D/%U` 라서, 빠뜨리면 `[homes]` 가
  엉뚱한 경로를 서비스하고 사용자는 빈 홈을 본다. 반드시 명시할 것.
- `template shell` 은 기본값이 이미 `/bin/false` 이므로(로그인 셸이 아니다) 빠뜨려도 셸이 열리지는 않는다.
  명시하는 이유는 위험해서가 아니라 plan.md §1 의 "SMB 전용" 3종 세트를 설정에 드러내 두기 위해서다.
- 조인 전제: DNS가 DC를 가리킬 것, 시각 동기(Kerberos ±5분), 머신 계정 생성 권한.

> winbind 옵션명은 배포 대상 Samba 버전 문서로 재확인할 것. 특히 `unix_primary_group`과 `template homedir`의 상호작용은 컨테이너에서 리허설을 권한다 — `internal/integration`에 이미 통합 테스트 하네스가 있으므로 Samba AD DC 컨테이너를 붙이면 실제로 재현할 수 있다.

---

## 7. 전환 절차

**데이터 이전(스토리지 교체)과 AD 전환을 같이 하지 말 것.** 문제가 생겼을 때 원인 분리가 불가능해진다.

### Step 0 — 데이터 이전이 있다면 먼저

uid 의미가 바뀌지 않는 동안 하는 것이 검증이 쉽다.

```sh
rsync -aHAX --numeric-ids --delete src/ dst/   # ★ --numeric-ids 없으면 이름 기준 매핑 → 조용한 오염
find /research -printf '%U %G %#m %p\n' | sort | sha256sum   # 전/후 동일해야 함
```
ZFS→ZFS면 `zfs send | zfs recv`가 더 안전하다.

### Step 1 — AD에 계정 생성 + 번호 주입

§3의 `export --format csv` → PowerShell. 서버는 아직 아무것도 안 바뀌므로 여기서 실수해도 무해하다.

### Step 2 — 조인하고 확인만

```sh
net ads join -U administrator
wbinfo -u                # 도메인 사용자가 보이는가
getent passwd skim       # ★ uid가 3001인가 — 이거 하나면 전환은 사실상 끝
```

`nsswitch.conf`를 `passwd: files winbind` / `group: files winbind`로 두면 **로컬 계정이 우선**한다. 즉 조인해도 기존 동작이 그대로 유지된다.

### Step 3 — 한 명씩 인계

로컬 항목을 지워야 winbind가 그 이름을 넘겨받는다. 이것이 `detach`다.

```sh
usersync detach --keep-upg skim   # --keep-upg 는 Step 5 참고
```

`detach`는 로컬 passwd/UPG/tdbsam 항목만 지우고 **홈 디렉터리는 그대로 둔다.** 그리고 지운 직후 이름을 다시 조회해:

- 같은 uid로 해석되면 → 인계 성공
- 아무것도 해석하지 않으면 → 경고 (조인 전이라면 정상)
- **다른 uid로 해석되면 → 에러.** 이름과 파일이 분리된 상태이므로 즉시 되돌려야 한다

roster가 그 사용자를 계속 선언하고 있어야 실행되며(그게 uid 예약이자 복구 경로다), 되돌리려면 `usersync apply` 한 번이면 로컬 계정이 재생성된다.

### Step 4 — 소유권 이전 선언

전원 인계가 끝나면 `usersync.yaml`에 `mode: audit`을 넣는다. 이때부터 `apply`/`purge`는 거부되고, usersync는 **장부 감시자**로 남는다.

```sh
usersync audit          # roster와 실제 해석 결과가 일치하는가. cron으로 상시
```

### Step 5 — UPG 이름 해석 (선택)

`gidNumber = uid`를 AD에 넣으면 번호는 보존되지만, 그 gid에 대응하는 **그룹 객체가 없어 `ls -l`이 이름 대신 숫자를 보여준다.** 세 갈래:

- **(권장) `group: files winbind`를 유지하고 로컬 `/etc/group`의 UPG 항목만 남긴다** — 이름 해석 복구, 비용 거의 0.
  단 **Step 3 에서 `usersync detach --keep-upg` 를 써야 한다.** 기본 `detach` 는 UPG 도 함께 지우므로,
  그냥 진행한 뒤 이 방법을 택하려면 `groupadd -g <uid> <name>` 으로 전원 분량을 손으로 되살려야 한다.
- AD에 사용자당 그룹 객체 생성 — 정석이지만 객체 수가 두 배
- UPG 포기 — 홈이 `0700`이라 권한상 무해하나 기존 파일의 gid가 미아가 된다

---

## 8. 롤백

각 단계는 되돌릴 수 있고, 뒤로 갈수록 비용이 커진다.

| 단계 | 되돌리는 법 |
| ---- | ----------- |
| Step 4 (`mode: audit`) | `mode: manage`로 되돌린다 |
| Step 3 (`detach`) | `usersync apply` — roster가 여전히 선언하므로 로컬 계정이 재생성된다 |
| Step 2 (조인) | `net ads leave` + `smb.conf.bak` 복원 |
| Step 1 (AD 속성) | 서버에 영향 없음 |

`usersync shares --write`는 이미 `testparm` 선검증 후 `.bak`을 남긴다.

---

## 9. 깨지는 것 (솔직하게)

1. **비밀번호는 못 옮긴다.** tdbsam의 NT 해시를 지원되는 방식으로 AD에 이관할 수 없다. 사용자는 전환 시점에 도메인 비밀번호로 갈아탄다 — 실질적으로는 "파일서버 전용 비번 → 회사 계정 통합"이라 사용자 경험은 **개선**된다. 공지 이벤트 하나로 처리한다. 시드 파생 초기 비밀번호 기능은 이 시점에 역할을 다한다.

2. **UPG 이름 해석** — §7 Step 5.

3. **winbind가 새 단일 장애점이 된다.** DC 불통 시 `getpwnam`이 실패하고, 파일 접근 전체가 멈춘다. `winbind offline logon`과 캐시 동작을 가용성 항목에 넣을 것.

4. **`usersync purge`는 여전히 shadow-utils 전용이다.** `userdel`을 직접 호출하므로 busybox/pw에서는 동작하지 않는다(`detach`는 `Provider.RemoveAccount`를 거치므로 세 백엔드 모두 지원). 파일 서버는 shadow-utils라 당장 문제는 없으나, 정리 대상으로 기록해 둔다.

---

## 10. 미결 — IT 확인 필요

- [ ] 도입 대상이 **온프렘 AD DS**인가? (§5 — 이 답에 따라 로드맵 절반이 달라진다)
- [ ] Entra Connect 동기화를 함께 계획하는가?
- [ ] 직원 단말을 **도메인 조인**할 계획인가? (조인하지 않으면 AD를 도입해도 SMB 프롬프트는 남는다)
- [ ] uid/gid **3000–19999 대역 예약**에 합의했는가? (§4)
- [ ] AD 명명 규칙(`sAMAccountName`)을 확정했는가? (§4)
