# SPEC: terraform-provider-zabbix v0.2 - hardening

Status: IMPLEMENTED (branch `hardening-v0.2`)
Data: 2026-08-30
Autorzy: Claude (draft + implementacja), recenzja GLM 5.3 Flash i Codex (gpt-5.6-terra),
fakty o API zweryfikowane na Zabbix 6.4.21 (docker-compose).

## 1. Cel

Doprowadzic provider z prototypu do stanu, w ktorym mozna nim bezpiecznie zarzadzac
produkcyjnym Zabbixem: stan Terraform nie ginie przy bledach transportowych, update
hosta nie niszczy konfiguracji, istniejace obiekty da sie zaimportowac, a regresje
wykrywa CI.

## 2. Zakres

W zakresie (v0.2):
- klient JSON-RPC: semantyka bledow, context, sesja, auth tokenem, TLS
- 4 istniejace zasoby: `zabbix_host_group`, `zabbix_host`, `zabbix_media_type`, `zabbix_action`
- import, walidacja schematu w planie, ForceNew, Sensitive
- testy jednostkowe + akceptacyjne, CI na PR
- dokumentacja zasobow (`docs/`) generowana `tfplugindocs`

Poza zakresem (v0.3+), decyzje z recenzji:
- nowe zasoby (`zabbix_template`, `zabbix_trigger`, `zabbix_user_group`, `zabbix_item`), data sources
- `recovery_operations` / `update_operations` akcji, `operationtype` inny niz "send message",
  `evaltype = 3` (custom formula), inne `eventsource` niz trigger
- `message_templates` media type (subject/message w operacji akcji pokrywa potrzebe)
- wiele interfejsow hosta, interfejsy SNMP/IPMI/JMX, makra, tagi, proxy, inventory
- parametry media type typu Script (`sortorder`/`value`)
- migracja SDKv2 -> terraform-plugin-framework (osobny etap po hardeningu)
- publikacja w Registry (namespace `adi` i module path do ustalenia z wlascicielem repo)

Wersja docelowa API: Zabbix 6.4. Naglowek `Authorization: Bearer` (wspierany od 6.4)
zamiast pola `auth` w body - kompatybilny z 7.x.

## 3. Wymagania i status

Kazde wymaganie ma priorytet i sprawdzalne kryterium akceptacji (AC). Status:
DONE = zaimplementowane i pokryte testem wskazanym w AC.

### 3.1 Klient (`zabbix/client.go`)

| ID | P | Wymaganie | AC (test) | Status |
|---|---|---|---|---|
| C1 | P1 | `Get*` zwracaja `ErrNotFound` tylko przy pustym wyniku; inne bledy propagowane | `TestGetHostGroup_NotFoundVsError`, `TestCall_HTTPErrorIsNotNotFound` | DONE |
| C2 | P2 | `context.Context` przez `NewRequestWithContext`; anulowanie przerywa zadanie | `TestCall_ContextCancelled` | DONE |
| C3 | P2 | Re-login po `Session terminated, re-login, please.` tylko dla auth haslem, single-flight pod mutexem, dokladnie 1 `user.login` dla N rownoleglych wywolan; brak retry mutacji po bledzie transportu | `TestCall_ReloginOnceOnSessionExpiry`, `TestCall_NoReloginWithAPIToken` | DONE |
| C4 | P2 | `api_token` (Bearer), XOR z `username`+`password`; `apiinfo.version` bez auth z `[]` | `TestCall_BearerHeaderAndUnauthenticatedVersion`, `TestProviderConfigure_AuthValidation` | DONE |
| C5 | P2 | `tls_insecure`, `ca_cert_file` (konflikt), bledny/nieistniejacy PEM = blad, self-signed odrzucany bez CA i akceptowany z CA | `TestNewZabbixClient_TLS` | DONE |
| C7 | P1 | Zero logowania payloadow/sekretow (`os.Stderr`, `fmt.Fprint`, `log.`) | `grep -rnE "os.Stderr|os.Stdout|fmt.Fprint|\blog\." zabbix/*.go` pusty (bez testow) | DONE |
| C8 | P3 | `selectHostGroups` + mapowanie `hostgroups`; interfejsy z `type,main,useip,ip,dns,port` | `TestAccHost_lifecycle` (grupy w stanie po imporcie) | DONE |
| C9 | P2 | Serializacja type-aware: wysylane tylko pola wlasciwe dla `type`, `parameters` nigdy `null`, `passwd` tylko przy `smtp_authentication=1` | `TestMediaTypeParams_TypeAware` | DONE |
| C10 | P2 | `host.update` nigdy nie wysyla `interfaces`; `templates_clear` = stare - nowe | `TestUpdateHost_NoInterfacesAndTemplatesClear` | DONE |
| C11 | P2 | `eventsource` tylko w `action.create`; `operations`/`conditions` zawsze tablice | `TestActionParams_EventSourceOnlyOnCreate` | DONE |

Odrzucone z draftu: C6 (lazy login - `terraform validate` nie konfiguruje providera,
wymaganie bylo sprzeczne z `GetVersion` w configure), `user.logout` (SDKv2 nie ma hooka
zamkniecia), flaga `-debug`/`-version` (YAGNI; `version` trafia do `User-Agent`).

### 3.2 Zasoby - wspolne

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| R1 | P1 | Read: `ErrNotFound` -> `SetId("")` + `tflog.Warn`; inny blad -> diag, ID zachowane; bledy `d.Set`/`Atoi` -> diag | `TestReadError` | DONE |
| R2 | P2 | Delete idempotentny: blad "does not exist" uznany za sukces dopiero po `Get` potwierdzajacym `ErrNotFound` | `TestDeleteError` | DONE |
| R3 | P1 | `Importer` passthrough x4, ID = natywny ID Zabbixa | `ImportStateVerify` w kazdym `TestAcc*` | DONE |
| R4 | P2 | `timeouts` (2 min default per CRUD); SDKv2 naklada deadline na ctx przekazywany do HTTP | `TestProvider_InternalValidate` + C2 | DONE |

Ryzyko zaakceptowane (Codex): pusty wynik przy utracie uprawnien tokenu jest
nieodroznialny od usuniecia obiektu - Zabbix zwraca pusta liste w obu przypadkach.
Zachowanie standardowe dla providerow Terraform; opisane w README i `ErrNotFound`.

### 3.3 `zabbix_host`

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| H1 | P1 | Read/Update uzywaja interfejsu `type=1 && main=1`; update przez `hostinterface.update`; pozostale interfejsy nietkniete; brak interfejsu agenta + zmiana `ip/dns/port` = blad (nie cichy stan posredni) | `TestAccHost_lifecycle` (SNMP dodany poza TF przetrwal 2 update'y) | DONE |
| H2 | P2 | `use_ip` (default true), `ip` Optional, `dns`; `CustomizeDiff`: `use_ip` wymaga `ip`, inaczej `dns`; create wysyla oba pola | `TestHostCustomizeDiff`, krok DNS w `TestAccHost_lifecycle` | DONE |
| H3 | P2 | `name` (Computed, default = `host`), `enabled`, `description`; `groups` `MinItems: 1`; `port` walidowany (1-65535 lub makro) | `TestHostCustomizeDiff`, `TestAccHost_lifecycle` | DONE |
| H4 | P3 | Odlaczenie template przez `templates_clear` (roznica stare - nowe), drugi template zostaje | krok 2 `TestAccHost_lifecycle` (2 -> 1 template) | DONE |

### 3.4 `zabbix_media_type`

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| M1 | P2 | Webhook bez `parameter` + zmiana nazwy = OK (`parameters: []`) | krok 2 `TestAccMediaType_webhook` | DONE |
| M2 | P2 | `parameters` czytane jako pole `output` (nie `selectParameters`); Read zawsze ustawia `parameter`; usuniecie parametrow poza TF jest wykrywane i naprawiane | krok 4 `TestAccMediaType_webhook` | DONE |
| M3 | P2 | `parameter.value` i `password` Sensitive; `script` NIE (to kod, sekrety maja byc w parametrach - udokumentowane) | schema; docs | DONE |
| M4 | P2 | Email: `smtp_port`, `smtp_security`, `smtp_verify_peer/host`, `smtp_authentication`, `username`, `password` -> API `passwd` (API zwraca `passwd`, wiec round-trip i `ImportStateVerify` dzialaja) | `TestAccMediaType_email` | DONE |
| M5 | P3 | Walidacja w planie: `type` w {0,1,2,4}; pola wymagane per typ; `parameter` tylko dla 4; `password` tylko z `smtp_authentication=1`; `timeout` 1-60s | `TestMediaTypeCustomizeDiff` | DONE |
| M6 | P3 | Read ustawia tylko pola wlasciwe dla typu, reszta = defaulty (brak perpetual diff po zmianie typu) | `resourceMediaTypeRead` | DONE |

### 3.5 `zabbix_action`

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| A1 | P2 | `eventsource` ForceNew, tylko 0, nie wysylane w update | `TestActionCustomizeDiff`, C11 | DONE |
| A2 | P2 | `condition` jako `TypeSet`; expand z `*schema.Set` | `TestAccAction_lifecycle` (3 warunki w innej kolejnosci niz API, plan pusty) | DONE |
| A3 | P2 | `operationtype` tylko 0; `evaltype` w {0,1,2}; kazda operacja >= 1 odbiorca; `subject`/`message` tylko z `default_msg=false` (API odrzuca inaczej); `esc_step_to` 0 lub >= `esc_step_from`; `esc_period` 0 lub >= 60s | `TestActionCustomizeDiff`, `TestParseZabbixDuration` | DONE |
| A4 | P2 | `users` -> `opmessage_usr`; `opmessage_grp`/`opmessage_usr` ZAWSZE wysylane (rowniez puste) - Zabbix zachowuje starych odbiorcow gdy pole pominiete (potwierdzone testem) | krok 2 `TestAccAction_lifecycle` (grupy usuniete, user zostaje) | DONE |
| A5 | P3 | `pause_suppressed`, `notify_if_canceled` jako pola akcji (nie operacji) | round-trip w `TestAccAction_lifecycle` | DONE |
| A6 | P3 | Read: przy `default_msg=1` `subject`/`message` = "" (Zabbix trzyma stare wartosci, sa bez znaczenia) | krok 2 `TestAccAction_lifecycle` | DONE |

### 3.6 Provider

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| P1 | P2 | Walidacja URL (http/https); warning przy `http://` poza loopback; warning przy wersji innej niz 6.4.x | `TestProviderConfigure_AuthValidation`, `TestProviderConfigure_WarnsOnPlainHTTPAndVersion` | DONE |
| P2 | P3 | `version` z ldflags -> `User-Agent` | `main.go`, `rawCall` | DONE |

Decyzja: HTTPS NIE jest wymuszane (Codex proponowal opt-in). Warning wystarcza;
twarde wymaganie zlamaloby lokalny workflow docker-compose bez realnego zysku.

### 3.7 Testy i CI

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| T1 | P1 | Testy jednostkowe na `httptest` (bez sieci) | `go test ./...` < 5 s | DONE |
| T2 | P1 | Akceptacyjne `resource.Test` per zasob: create/update/import/destroy + scenariusze z AC; unikalne nazwy (`acctest.RandomWithPrefix`); `CheckDestroy` | `TF_ACC=1 go test ./zabbix -run TestAcc` zielone na 6.4.21 | DONE |
| T3 | P1 | CI na push/PR: gofmt, vet, `go mod tidy` check, `go test -race`, docs drift, job acceptance na docker-compose (`--wait` + healthcheck); actions pinowane po SHA | `.github/workflows/ci.yml` | DONE (do potwierdzenia na GitHub po push) |
| T4 | P3 | docker-compose: bez `version:`, obrazy `alpine-6.4.21`, healthchecki, porty na 127.0.0.1 | `docker compose config` | DONE |

### 3.8 Dokumentacja

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| D1 | P2 | `docs/` przez `go generate` (tfplugindocs), `examples/` z `resource.tf` + `import.sh`; CI sprawdza drift | `docs/resources/*.md` x4 + `docs/index.md` | DONE |
| D2 | P3 | README: 4 zasoby, `api_token`, TLS, zachowania (Read/interfejsy/templates_clear), akceptacja, override dla Windows, CHANGELOG | `README.md`, `CHANGELOG.md` | DONE |

## 4. Fakty o API 6.4.21 ustalone empirycznie

- `Authorization: Bearer <token>` dziala dla sesji i tokenow API.
- `host.get selectHostGroups` zwraca klucz `hostgroups` (nie `groups`).
- `mediatype.get`: `parameters` to zwykle pole `output`; `selectParameters` jest ignorowane
  (stary kod nigdy nie ladowal parametrow do stanu). `passwd` zwracany jawnie.
- `mediatype.update` webhooka z `parameters: null` -> `Invalid parameter "/1/parameters": an array is expected.`
- `action.get`: `opmessage` jest obiektem (nie tablica); `pause_suppressed` na poziomie akcji.
- `action.update`: operacja bez `opmessage_grp` zachowuje stare grupy; `subject`/`message`
  przy `default_msg=1` -> `unexpected parameter "subject"`; brak odbiorcow ->
  `No recipients specified for action operation message.`
- Host moze miec jednoczesnie `main=1` dla SNMP i dla agenta (main jest per typ).
- `hostinterface.update` i `host.update templates_clear` dzialaja zgodnie z docs.
- Nieistniejacy obiekt w delete: `-32500 No permissions to referred object or it does not exist!`
- Zly/wygasly token: `-32602 Session terminated, re-login, please.` (nieodroznialne).

## 5. Ryzyka pozostale

- Utrata uprawnien = "not found" (patrz 3.2).
- `zabbix_host` bez interfejsu agenta (tylko SNMP) jest read-only w zakresie interfejsu;
  v0.3: blok `interface` (lista).
- Zabbix 6.4 jest EOL; 7.0 LTS nietestowane (warning). Bearer auth jest gotowe na 7.x.
- `esc_period` z makrem uzytkownika nie jest walidowane (nie da sie).
