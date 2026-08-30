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
- publikacja w Registry (sam cut-release); namespace ujednolicony na `Tensai123` (repo wydajace artefakty)

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
| C3 | P2 | Re-login po `Session terminated, re-login, please.` tylko dla auth haslem, single-flight (semafor respektujacy ctx, mutex tylko na token), dokladnie 1 `user.login` dla N rownoleglych wywolan; brak retry mutacji po bledzie transportu | `TestCall_ReloginOnceOnSessionExpiry`, `TestCall_NoReloginWithAPIToken` | DONE |
| C4 | P2 | `api_token` (Bearer), XOR z `username`+`password`; `apiinfo.version` bez auth z `[]` | `TestCall_BearerHeaderAndUnauthenticatedVersion`, `TestProviderConfigure_AuthValidation` | DONE |
| C5 | P2 | `tls_insecure` (env `ZABBIX_TLS_INSECURE` przez `DefaultFunc` + `ParseBool`, HCL ma pierwszenstwo), `ca_cert_file` (konflikt), bledny/nieistniejacy PEM = blad, self-signed odrzucany bez CA i akceptowany z CA; body odpowiedzi HTTP nigdy w diagnostyce | `TestNewZabbixClient_TLS`, `TestEnvBoolDefault` | DONE |
| C7 | P1 | Zero logowania payloadow/sekretow (`os.Stderr`, `fmt.Fprint`, `log.`) | `grep -rnE "os.Stderr|os.Stdout|fmt.Fprint|\blog\." zabbix/*.go` pusty (bez testow) | DONE |
| C8 | P3 | `selectHostGroups` + mapowanie `hostgroups`; interfejsy z `type,main,useip,ip,dns,port` | `TestAccHost_lifecycle` (grupy w stanie po imporcie) | DONE |
| C9 | P2 | Serializacja type-aware: wysylane tylko pola wlasciwe dla `type`, `parameters` nigdy `null`, `passwd` tylko przy `smtp_authentication=1` | `TestMediaTypeParams_TypeAware` | DONE |
| C10 | P2 | `host.update` nigdy nie wysyla `interfaces`; `templates_clear` = stare - nowe | `TestUpdateHost_NoInterfacesAndTemplatesClear` | DONE |
| C11 | P2 | `eventsource` tylko w `action.create`; `operations`/`conditions` zawsze tablice | `TestActionParams_EventSourceOnlyOnCreate` | DONE |
| C12 | P1 | Klient nie podaza za redirectami (307/308 powtorzyloby POST z tokenem, mozliwy downgrade do http) | `TestCall_RedirectsAreNotFollowed` | DONE |
| C13 | P1 | Odpowiedz 200 bez `result` i bez `error` (albo bez `jsonrpc: 2.0`) = blad, nigdy sukces mutacji | `TestCall_MalformedSuccessResponse` | DONE |
| C14 | P2 | URL z userinfo odrzucany (nie wycieka do diagnostyki); `api_token` weryfikowany w configure przez `user.get` | `TestProviderConfigure_AuthValidation` | DONE |
| C15 | P3 | Leniwe pierwsze logowanie tez single-flight | `TestCall_LazyFirstLoginIsSingleFlight` | DONE |
| C16 | P2 | Single-flight loginu dzieli wynik (takze blad) ze wszystkimi oczekujacymi - nieudany re-login nie powoduje N kolejnych prob; wynik publikowany i `flight` czyszczony atomowo pod mutexem | `TestCall_FailedReloginIsSharedByWaiters` | DONE |
| C17 | P2 | Brak timeoutu na `http.Client` - deadline zawsze z ctx (timeouts zasobu; `configureTimeout` 2 min dla probe'ow providera), wiec `timeouts { create = "15m" }` dziala | `TestNewZabbixClient_NoImplicitTimeout`, `TestCall_ContextCancelled` | DONE |
| C19 | P2 | Login single-flight dziala w tle na kontekscie odlaczonym od inicjatora (wlasny limit 60 s); kazdy wywolujacy, takze inicjator, czeka tylko do swojego deadline'u; nieudany login zapamietany 30 s dla tej samej generacji tokenu (spoznieni wywolujacy nie generuja kolejnych prob) | `TestCall_ReloginSurvivesInitiatorCancellation`, `TestCall_FailedLoginIsMemoisedForLateCallers` | DONE |
| C20 | P3 | `ca_cert_file` dokladany do systemowej puli CA, nie zamiast niej; `isLoopback` przez `net.ParseIP().IsLoopback()` (bez prefiksu `127.` dla nazw DNS) | `TestNewZabbixClient_TLS`, `TestIsLoopback` | DONE |
| C18 | P2 | Mutacja nigdy nie jest ponawiana po bledzie transportu (zerwane polaczenie = dokladnie 1 request, `req.GetBody = nil` wylacza tez transparentne retry net/http na reused connection); body odpowiedzi HTTP nie trafia do diagnostyki (test z echem tokenu) | `TestCall_MutationIsNeverRetriedOnTransportError`, `TestCall_HTTPErrorIsNotNotFound` | DONE |
| C21 | P2 | Koperta JSON-RPC (wersja 2.0 + ID zadania, dokladnie jedno z `result`/`error`) walidowana przed klasyfikacja bledu - sfalszowana odpowiedz nie moze wymusic re-loginu i ponowienia mutacji | `TestCall_ForgedErrorEnvelopeDoesNotTriggerRelogin`, `TestCall_MalformedSuccessResponse` | DONE |
| C22 | P3 | URL bez query/fragmentu (sekrety w query nie trafiaja do diagnostyk); mnozenie czasu odporne na overflow | `TestProviderConfigure_AuthValidation`, `TestParseZabbixDuration` | DONE |
| C23 | P2 | Konflikt `tls_insecure`/`ca_cert_file` sprawdzany po rozwiazaniu defaultow ze srodowiska (nie `ConflictsWith`): sam `ca_cert_file` w HCL dziala, a `ZABBIX_TLS_INSECURE=true` nie omija konfliktu | `TestProviderConfigure_AuthValidation`, `TestEnvBoolDefault` | DONE |
| C25 | P3 | `api_token` w HCL wygrywa z ZABBIX_USERNAME/PASSWORD ze srodowiska (warning zamiast twardego bledu - typowe w CI) | `TestProviderConfigure_TokenWinsOverEnvCredentials` | DONE |
| C24 | P3 | Konstruktor zadan `newSingleShotRequest` wydzielony i asertowany wprost (`GetBody == nil`) | `TestRawCall_RequestsAreNotReplayable` | DONE |

Odrzucone z draftu: C6 (lazy login - `terraform validate` nie konfiguruje providera,
wymaganie bylo sprzeczne z `GetVersion` w configure), `user.logout` (SDKv2 nie ma hooka
zamkniecia), flaga `-debug`/`-version` (YAGNI; `version` trafia do `User-Agent`).

### 3.2 Zasoby - wspolne

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| R1 | P1 | Read: `ErrNotFound` -> `SetId("")` + diag Warning widoczny w planie; inny blad -> diag, ID zachowane; bledy `d.Set`/`Atoi` -> diag | `TestReadError` | DONE |
| R2 | P2 | Delete idempotentny: blad "does not exist" uznany za sukces dopiero po `Get` potwierdzajacym `ErrNotFound` | `TestDeleteError` | DONE |
| R3 | P1 | `Importer` passthrough x4, ID = natywny ID Zabbixa | `ImportStateVerify` w kazdym `TestAcc*` | DONE |
| R4 | P2 | `timeouts` (2 min default per CRUD); SDKv2 naklada deadline na ctx przekazywany do HTTP; limit HTTP klienta 5 min tylko jako siatka bezpieczenstwa (linkowanie duzych template'ow na wolnym serwerze przekraczalo 30 s w CI) | `TestProvider_InternalValidate` + C2 | DONE |

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
| H6 | P2 | `name` niekonfigurowane = zawsze rowne `host`; normalizacja w `CustomizeDiff` (`SetNew`), wiec zmiana nazwy widocznej poza TF pokazuje sie w planie | `TestHostCustomizeDiff_NameFollowsHostUnlessConfigured`, krok DNS `TestAccHost_lifecycle` | DONE |
| H8 | P3 | `ip` walidowane w planie (`net.ParseIP` lub makro uzytkownika) | `TestHostCustomizeDiff` | DONE |
| H7 | P2 | `templates_clear` liczone z aktualnego stanu API (template podpiety poza TF jest czyszczony, nie tylko odlaczany) | `TestHostUpdate_TemplatesClearFromAPIState` | DONE |
| H5 | P2 | Host bez interfejsu agenta (zaimportowany SNMP-only lub interfejs usuniety poza TF): Read pokazuje drift (puste `ip`/`dns`), a Update tworzy interfejs agenta z konfiguracji (`hostinterface.create`); interfejs SNMP nietkniety | `TestHostRead_NoAgentInterfaceShowsDrift`, `TestHostUpdate_NoAgentInterface`, `TestHostCustomizeDiff_ImportedHostWithoutAgentInterface` | DONE |

### 3.4 `zabbix_media_type`

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| M1 | P2 | Webhook bez `parameter` + zmiana nazwy = OK (`parameters: []`) | krok 2 `TestAccMediaType_webhook` | DONE |
| M2 | P2 | `parameters` czytane jako pole `output` (nie `selectParameters`); Read zawsze ustawia `parameter`; usuniecie parametrow poza TF jest wykrywane i naprawiane | krok 4 `TestAccMediaType_webhook` | DONE |
| M3 | P2 | `parameter.value` i `password` Sensitive; `script` NIE (to kod, sekrety maja byc w parametrach - udokumentowane) | schema; docs | DONE |
| M4 | P2 | Email: `smtp_port`, `smtp_security`, `smtp_verify_peer/host`, `smtp_authentication`, `username`, `password` -> API `passwd` (API zwraca `passwd`, wiec round-trip i `ImportStateVerify` dzialaja) | `TestAccMediaType_email` | DONE |
| M5 | P3 | Walidacja w planie: `type` w {0,1,2,4}; pola wymagane per typ; `parameter` tylko dla 4; `password` tylko z `smtp_authentication=1`; `timeout` 1-60s | `TestMediaTypeCustomizeDiff` | DONE |
| M6 | P3 | Read ustawia tylko pola wlasciwe dla typu, reszta = defaulty (brak perpetual diff po zmianie typu) | `TestMediaTypeRead_TypeAwareReset` | DONE |
| M7 | P2 | Update wysyla pola nieaktywnych typow wyczyszczone (zmiana typu nie zostawia np. `passwd` w Zabbixie - potwierdzone na API) | `TestMediaTypeParams_TypeAware`, `TestAccMediaType_scriptSmsTypeChange` | DONE |
| M8 | P2 | Jawnie ustawione pola innego typu odrzucane w planie (po raw config, wiec takze wartosc rowna defaultowi); walidacja per pole, wartosci unknown traktowane jako ustawione; `smtp_authentication = 1` wymaga `username` i `password` w planie | `TestMediaTypeCustomizeDiff` | DONE |
| M9 | P1 | Script media type: `parameters` (sortorder/value) nigdy nie wysylane; Read odmawia zarzadzania skryptem z parametrami (inaczej rename kasowalby argumenty) | `TestMediaTypeParams_ScriptParametersUntouched`, `TestMediaTypeRead_RefusesScriptWithParameters` | DONE |
| M10 | P3 | `timeout` webhooka: tylko 1-60s, makra odrzucane (API ich nie przyjmuje) | `TestMediaTypeCustomizeDiff` | DONE |
| M12 | P1 | Zmiana typu na Script wysyla `parameters: []` (inaczej parametry webhooka zostawaly i Read odmawial zarzadzania) | `TestMediaTypeParams_ScriptParametersUntouched`, kroki webhook->script w `TestAccMediaType_scriptSmsTypeChange` | DONE |
| M11 | P3 | Read: najpierw walidacja `type` (nieobslugiwany = odmowa z hintem), potem parsowanie WYLACZNIE pol wlasciwych dla typu; puste wartosci pol obcych tolerowane (obiekty spoza providera), nieparsowalne pole wlasnego typu = odmowa z hintem `terraform state rm` | `TestMediaTypeRead_RefusesUnsupportedType`, `TestMediaTypeRead_ForeignNumericFieldsAreTolerated`, `TestMediaTypeRead_OwnNonNumericFieldRefusedWithHint` | DONE |

### 3.5 `zabbix_action`

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| A1 | P2 | `eventsource` ForceNew, tylko 0, nie wysylane w update | `TestActionCustomizeDiff`, C11 | DONE |
| A2 | P2 | `condition` jako `TypeSet`; expand z `*schema.Set` | `TestAccAction_lifecycle` (3 warunki w innej kolejnosci niz API, plan pusty) | DONE |
| A3 | P2 | `operationtype` tylko 0; `evaltype` w {0,1,2}; kazda operacja >= 1 odbiorca; `subject`/`message` tylko z `default_msg=false` (API odrzuca inaczej); `esc_step_to` 0 lub >= `esc_step_from`; `esc_period` 0 lub >= 60s | `TestActionCustomizeDiff`, `TestParseZabbixDuration` | DONE |
| A4 | P2 | `users` -> `opmessage_usr`; `opmessage_grp`/`opmessage_usr` ZAWSZE wysylane (rowniez puste) - Zabbix zachowuje starych odbiorcow gdy pole pominiete (potwierdzone testem) | krok 2 `TestAccAction_lifecycle` (grupy usuniete, user zostaje) | DONE |
| A5 | P3 | `pause_suppressed`, `notify_if_canceled` jako pola akcji (nie operacji) | round-trip w `TestAccAction_lifecycle` | DONE |
| A6 | P3 | Read: przy `default_msg=1` `subject`/`message` = "" (Zabbix trzyma stare wartosci, sa bez znaczenia) | krok 2 `TestAccAction_lifecycle`, `TestActionRead_DefaultMsgHidesStaleSubject` | DONE |
| A7 | P2 | Read odmawia zarzadzania akcja z `operationtype != 0` (update nadpisuje cala liste operacji - cicha utrata) | `TestActionRead_RefusesUnsupportedOperationType` | DONE |
| A8 | P2 | `condition.value2` (nazwa tagu dla typu 26) wysylane tylko dla typu 26 (API odrzuca dla innych) i walidowane | `TestActionRead_Mapping`, `TestActionCustomizeDiff`, warunek typu 26 w `TestAccAction_lifecycle` | DONE |
| A9 | P2 | Read odmawia zarzadzania akcja z `eventsource != 0` lub `evaltype = 3` (custom formula bylaby cicho skasowana) | `TestActionRead_RefusesUnsupportedEventSourceAndEvaltype` | DONE |
| A10 | P2 | `CustomizeDiff` (host, media type, action) pomija walidacje wartosci nieznanych w planie (referencje do innych zasobow) | `TestCustomizeDiff_UnknownValuesAreDeferred` | DONE |
| A11 | P1 | Read odmawia zarzadzania operacja z `opconditions` (update nadpisuje operacje w calosci) ; komunikaty odmowy wskazuja `terraform state rm` | `TestActionRead_RefusesOperationConditions` | DONE |
| A13 | P2 | `operation` wymagane (`MinItems: 1`, jak w Zabbixie); `esc_period` akcji 60s-1w (0 tylko w operacji); kolejnosc operacji = kolejnosc konfiguracji, test z 2 operacjami bez perpetual diff | `TestActionCustomizeDiff`, `TestAccAction_lifecycle` | DONE |
| A14 | P1 | Macierz warunkow zgodna z 6.4 zweryfikowana empirycznie (`action.create` na 6.4.21): `conditiontype` w {0,1,2,3,4,6,13,25,26} (5/15/16 odrzucane przez API), operatory per typ (np. event name tylko contains/not-contains, severity =,!=,>=,<=, time period in/not-in); zle pary odrzucane w planie, a nieznane wartosci z importu = odmowa z hintem | `TestActionCustomizeDiff`, `TestActionRead_RefusesUnknownConditionOperator` | DONE |
| A12 | P2 | Elementy setu `condition` z wartosciami unknown (marker SDK zamiast typu) nie powoduja paniki ani falszywych bledow w planie | `TestCustomizeDiff_UnknownValuesAreDeferred` | DONE |

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
| T3 | P1 | CI na push/PR: gofmt, vet, `go mod tidy` check, `go test -race`, docs drift, job acceptance na docker-compose (`--wait` + healthcheck); actions pinowane po SHA, terraform pinowany; release wywoluje CI (`workflow_call`) jako bramke, GoReleaser pinowany | `.github/workflows/ci.yml`, `release.yml`; run 33322303435 na forku SychPL zielony (unit + acceptance) | DONE |
| T5 | P2 | Testy jednostkowe mapowania API -> stan (Read) na fixture'ach z realnych odpowiedzi 6.4 | `resource_read_test.go` | DONE |
| T6 | P2 | CI: `govulncheck` (zaleznosci i Go podniesione - 0 znanych podatnosci), `terraform fmt -check examples/`, `goreleaser check` + `build --snapshot --single-target`; release wymaga tagu osiagalnego z `main` i environment `release` | `ci.yml`, `release.yml` | DONE |
| T7 | P3 | `.goreleaser.yml`: usunieta niewspierana para darwin/arm, `archives.formats`, manifest Registry (`terraform-registry-manifest.json`) w release i w checksumie | `goreleaser check` w CI | DONE |
| T8 | P3 | CI acceptance: czeka na start zabbix-server (locki DB przy pierwszym starcie blokowaly `host.create`), lekkie template'y w tescie hosta, zrzut logow przy failu, obrazy przypiete digestem | `ci.yml`, `docker-compose.acc.yml` | DONE |
| T10 | P3 | CI: `terraform validate` wszystkich `examples/` i `example_deployment/` na zbudowanym providerze (dev override); usuniety hook `go mod tidy` z goreleasera; docs drift lapie tez pliki nieznane gitowi; snapshot build wszystkich targetow | `ci.yml` | DONE |
| T13 | P2 | Testy jednostkowe CRUD na poziomie zasobow (nie tylko klienta): host_group create/update/delete, action update z czyszczeniem odbiorcow, media type delete idempotentny | `TestHostGroupResource_CRUD`, `TestActionResource_UpdateSendsClearedRecipients`, `TestMediaTypeResource_DeleteIdempotent` | DONE |
| T14 | P3 | CI: `staticcheck` (pinowany), `concurrency` z `cancel-in-progress`; template docs opisuje wykluczajace sie atrybuty (tfplugindocs ich nie renderuje) | `ci.yml`, `templates/index.md.tmpl` | DONE |
| T12 | P3 | Testy: domyslne timeouty 2 min asertowane dla 4 zasobow; `filter.conditions` zawsze tablica; sekret z URL nigdy w zadnej diagnostyce; `pause_suppressed`/`notify_if_canceled` = false round-trip w akceptacji | `TestResourceTimeoutsDefaults`, `TestActionParams_EventSourceOnlyOnCreate`, `TestProviderConfigure_AuthValidation`, `TestAccAction_lifecycle` | DONE |
| T11 | P2 | CI acceptance: `ANALYZE` swiezej bazy przed testami - bez statystyk planera zapytania `templates_clear` trwaly minuty (potwierdzone `pg_stat_activity`), hosty testowe tworzone jako wylaczone | `ci.yml`, run 33327064286 zielony | DONE |
| T9 | P2 | CI egzekwuje C7: blokujacy grep na `os.Stderr/Stdout`, `fmt.Fprint/Print`, `log.` w kodzie providera; `example_deployment/` w `terraform fmt -check` | `ci.yml` | DONE |
| T4 | P3 | `docker-compose.acc.yml` (upstream ignoruje `docker-compose.yml` jako plik lokalny): bez `version:`, obrazy `alpine-6.4.21`, healthchecki, porty na 127.0.0.1 | `docker compose -f docker-compose.acc.yml config` | DONE |

### 3.8 Dokumentacja

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| D1 | P2 | `docs/` przez `go generate` (tfplugindocs), `examples/` z `resource.tf` + `import.sh`; CI sprawdza drift | `docs/resources/*.md` x4 + `docs/index.md` | DONE |
| D2 | P3 | README: 4 zasoby, `api_token`, TLS, zachowania (Read/interfejsy/templates_clear), sekcja o obiektach nieobslugiwanych (`terraform state rm`), akceptacja, override dla Windows, CHANGELOG | `README.md`, `CHANGELOG.md` | DONE |
| D3 | P2 | Opisy zasobow (i wygenerowane docs) ostrzegaja o autorytatywnym zarzadzaniu przy imporcie (templates/filter/operacje/atrybuty typu trzeba odtworzyc w konfiguracji przed pierwszym apply); przyklady bez hardcodowanych ID instancji (zmienne) | `docs/resources/*.md`, `examples/` | DONE |
| D4 | P3 | CI: `timeout-minutes` na jobach; marginesy czasowe testow logowania powiekszone (stabilnosc na wolnych runnerach) | `ci.yml`, `client_test.go` | DONE |

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

## 4a. Uwagi recenzentow odrzucone (z uzasadnieniem)

- (Codex, r7) "Redagowac `message`/`data` bledow JSON-RPC w diagnostyce": tresc bledu API to
  podstawowa diagnostyka providera (kazdy provider Terraform ja pokazuje); koperta odpowiedzi
  jest walidowana (C21), a body HTTP nie-JSON-RPC nigdy nie trafia do bledow (C18).
- (Codex, r5) "Wymusic HTTPS": warning przy nie-loopbackowym http (P1) zamiast twardej blokady -
  swiadomy wybor, lokalne laby i test acceptance chodza po http.
- (GLM, r7) "Wspoldzielona mapa schematu media type": jedna instancja providera na proces,
  SDK nie mutuje schematu po InternalValidate; przebudowa per wywolanie bez zysku.

## 5. Ryzyka pozostale

- Utrata uprawnien = "not found" (patrz 3.2).
- `TestAccProvider_APIToken` wymaga poswiadczen haslem (mintuje token); przy uruchomieniu
  akceptacji samym tokenem jest pomijany.
- `zabbix_host` zarzadza tylko glownym interfejsem agenta (tworzy go, gdy brak); pozostale interfejsy sa nietykane;
  v0.3: blok `interface` (lista).
- Zabbix 6.4 jest EOL; 7.0 LTS nietestowane (warning). Bearer auth jest gotowe na 7.x.
- `esc_period` z makrem uzytkownika nie jest walidowane (nie da sie).
- Namespace ujednolicony na `Tensai123` (module path, `source`, docs, override). Jesli wlasciciel
  publikuje pod innym namespace, zmiana jest mechaniczna (grep `Tensai123/zabbix`).
- Environment `release` wymaga skonfigurowania required reviewers w ustawieniach repo, zeby faktycznie
  bramkowal uzycie klucza GPG.
