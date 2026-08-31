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
- publikacja w Registry (sam cut-release); namespace wydawniczy: `SychPL` (fork publikujacy artefakty; upstream `Tensai123` to konkurent - bez PR-ow)

Wersja docelowa API: Zabbix 6.4. Naglowek `Authorization: Bearer` (wspierany od 6.4)
zamiast pola `auth` w body - kompatybilny z 7.x. Od rundy 22 macierz akceptacji CI
obejmuje takze 7.0 LTS (6.4 jest EOL); `SupportedVersionPrefixes` = {6.4, 7.0}.

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
| C14 | P2 | URL z userinfo odrzucany (nie wycieka do diagnostyki); `api_token` weryfikowany w configure przez `user.checkAuthentication` z parametrem `token` (niezalezne od uprawnien roli do metod API; wolane bez naglowka Authorization - potwierdzone empirycznie na 6.4.21) | `TestProviderConfigure_AuthValidation` | DONE |
| C15 | P3 | Leniwe pierwsze logowanie tez single-flight | `TestCall_LazyFirstLoginIsSingleFlight` | DONE |
| C16 | P2 | Single-flight loginu dzieli wynik (takze blad) ze wszystkimi oczekujacymi - nieudany re-login nie powoduje N kolejnych prob; wynik publikowany i `flight` czyszczony atomowo pod mutexem | `TestCall_FailedReloginIsSharedByWaiters` | DONE |
| C30 | P2 | `CheckAuth` wymaga niepustego `userid` w odpowiedzi `user.checkAuthentication` - poprawna koperta z pustym obiektem (stub posrednika) nie uchodzi za zweryfikowany token (GLM r31) | `TestProviderConfigure_CheckAuthRequiresUserID` | DONE |
| C29 | P1 | Obiekt `error` bez obowiazkowych pol JSON-RPC (`code`/`message`) = malformed: spreparowane samo `data` z markerem wygasniecia sesji nie wymusi re-loginu i powtorki mutacji (Codex r26) | `TestCall_PartialErrorObjectDoesNotTriggerRelogin` | DONE |
| C27 | P2 | Koperta JSON-RPC: obecnosc pola `error` (nawet `null`) obok `result` = malformed, samotny `error: null` = malformed; `error` sledzone przez RawMessage (Codex r15) | `TestCall_NullErrorMemberIsMalformed` | DONE |
| C26 | P2 | `mutate` weryfikuje ID mutowanego obiektu (puste/obce ID = blad, nie sukces; create-style: lista samych pustych ID = blad); `firstID` odrzuca puste ID (Codex r13) | `TestMutate_RejectsEmptyAndForeignIDs` | DONE |
| C17 | P2 | Brak timeoutu na `http.Client` - deadline zawsze z ctx (timeouts zasobu; `configureTimeout` 2 min dla probe'ow providera), wiec `timeouts { create = "15m" }` dziala | `TestNewZabbixClient_NoImplicitTimeout`, `TestCall_ContextCancelled` | DONE |
| C19 | P2 | Login single-flight dziala w tle na kontekscie odlaczonym od inicjatora (wlasny limit 60 s); kazdy wywolujacy, takze inicjator, czeka tylko do swojego deadline'u; nieudany login zapamietany 30 s dla tej samej generacji tokenu (spoznieni wywolujacy nie generuja kolejnych prob) | `TestCall_ReloginSurvivesInitiatorCancellation`, `TestCall_FailedLoginIsMemoisedForLateCallers` | DONE |
| C20 | P3 | `ca_cert_file` dokladany do systemowej puli CA, nie zamiast niej; `isLoopback` przez `net.ParseIP().IsLoopback()` (bez prefiksu `127.` dla nazw DNS) | `TestNewZabbixClient_TLS`, `TestIsLoopback` | DONE |
| C18 | P2 | Mutacja nigdy nie jest ponawiana po bledzie transportu (zerwane polaczenie = dokladnie 1 request, `req.GetBody = nil` wylacza tez transparentne retry net/http na reused connection); body odpowiedzi HTTP nie trafia do diagnostyki (test z echem tokenu) | `TestCall_MutationIsNeverRetriedOnTransportError`, `TestCall_HTTPErrorIsNotNotFound` | DONE |
| C21 | P2 | Koperta JSON-RPC (wersja 2.0 + ID zadania, dokladnie jedno z `result`/`error`) walidowana przed klasyfikacja bledu - sfalszowana odpowiedz nie moze wymusic re-loginu i ponowienia mutacji | `TestCall_ForgedErrorEnvelopeDoesNotTriggerRelogin`, `TestCall_MalformedSuccessResponse` | DONE |
| C22 | P3 | URL bez query/fragmentu (sekrety w query nie trafiaja do diagnostyk); mnozenie czasu odporne na overflow | `TestProviderConfigure_AuthValidation`, `TestParseZabbixDuration` | DONE |
| C23 | P2 | Konflikt `tls_insecure`/`ca_cert_file` sprawdzany po rozwiazaniu defaultow ze srodowiska (nie `ConflictsWith`): sam `ca_cert_file` w HCL dziala, a `ZABBIX_TLS_INSECURE=true` nie omija konfliktu | `TestProviderConfigure_AuthValidation`, `TestEnvBoolDefault` | DONE |
| C25 | P3 | Jawna konfiguracja wygrywa ze srodowiskiem w OBU kierunkach (HCL token vs env credy i HCL credy vs env token) z warningiem; dwie jawne metody = blad. `ConflictsWith`/`RequiredWith` usuniete ze schematu - walidacja SDK odpala sie PO wstrzyknieciu defaultow z env i wywalala provider z samym api_token w HCL (potwierdzone testem przez `Provider().Validate`) | `TestProviderValidate_EnvDefaultsDoNotConflict`, `TestProviderConfigure_TokenWinsOverEnvCredentials`, `TestProviderConfigure_CredentialsWinOverEnvToken` | DONE |
| C26 | P2 | Kazda mutacja weryfikuje typowana odpowiedz (niepusta lista oczekiwanych ID) - `{"result": false}` albo pusta lista nigdy nie jest sukcesem | `TestMutate_RejectsResultsWithoutIDs` | DONE |
| C27 | P3 | Warning gdy `tls_insecure` aktywne (rowniez z env); memo nieudanego loginu wygasa i swieza proba przechodzi; `0s` rownowazne `0` w esc_period operacji; martwe pole `ClientConfig.Timeout` usuniete | `TestCall_FailedLoginMemoExpires`, `TestParseZabbixDuration` | DONE |
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
| R4 | P2 | `timeouts` (2 min default per CRUD); SDKv2 naklada deadline na ctx przekazywany do HTTP; klient HTTP celowo BEZ wlasnego timeoutu (C17, `TestNewZabbixClient_NoImplicitTimeout`) - deadline pochodzi wylacznie z ctx, wiec `timeouts { create = "15m" }` dziala | `TestProvider_InternalValidate` + C2 | DONE |
| R6 | P2 | Dependabot pilnuje takze digestow obrazow acceptance (`package-ecosystem: docker-compose`) - zamrozone 6.4.21/7.0.30/Postgres nie starzeja sie cicho (GLM r31) | konfiguracja `.github/dependabot.yml` | DONE |
| R5 | P3 | Domkniecia GLM r18: jawny blad przy odpowiedzi >32 MiB (zamiast mylacego bledu parsowania po cichym obcieciu); handshake `tls_insecure` testowany end-to-end; odmowa hostow LLD w opisie zasobu i README; marker unknown SDK skomentowany (hcl2shim.UnknownVariableValue jest internal - kopia swiadoma); krok acc zmieniajacy zbior `groups` | `TestNewZabbixClient_TLS`, krok cfgDNS w `TestAccHost_lifecycle` | DONE |

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
| H9 | P2 | Host bez zadnego interfejsu (puste `ip` i `dns`): create bez `interfaces`, dodanie adresu tworzy interfejs, usuniecie adresu z konfiguracji usuwa go (`hostinterface.delete`) - hosty na trapper/dependent items (wymaganie uzytkownika); niestandardowy `port` bez adresu odrzucany w planie (nigdy by nie konwergowal) | `TestHostResource_NoInterfaceLifecycle`, `TestAccHost_noInterface`, `TestHostCustomizeDiff` | DONE |
| H10 | P3 | Read odmawia zarzadzania hostem z LLD (`flags != 0`); `host.get` nie zwraca szablonow (sprawdzone empirycznie - import ID szablonu daje not found) | `TestHostRead_RefusesDiscoveredHost` | DONE |
| H7 | P2 | `templates_clear` liczone z aktualnego stanu API (template podpiety poza TF jest czyszczony, nie tylko odlaczany) | `TestHostUpdate_TemplatesClearFromAPIState` | DONE |
| T24 | P3 | Host w tescie pelnego poprzedniego stanu po nieudanym Apply (`Resource.Diff`+`Apply`, wszystkie stare pola zachowane); macierz acceptance asertuje wersje API, z ktora faktycznie rozmawia (`ZABBIX_ACC_EXPECT_VERSION`); wymaganie P1 spojne ze wspieranymi liniami 6.4/7.0 (Codex r34) | przypadek "host" w `TestPartialStateOnFailedUpdates`, `testAccPreCheck` | DONE |
| T23 | P3 | Import obiektu utworzonego POZA providerem (seed surowym API + import + pusty plan) pokryty akceptacyjnie; README dokumentuje sonde `apiinfo.version` przed konfiguracja oraz stockowe obiekty wymagane przez suite akceptacyjny (GLM r32) | `TestAccMediaTypeImport_ExternallyCreated` | DONE |
| T22 | P3 | `CheckDestroy` testu akceptacyjnego akcji obejmuje takze zasoby pomocnicze (media type webhook, obie grupy hostow) - regresja ich Delete nie przejdzie cicho (Codex r33) | `TestAccAction_lifecycle` | DONE |
| T21 | P3 | Idempotentny destroy akcji (objectMissing + potwierdzajacy get) pokryty testem na poziomie zasobu jak host/grupa/media type (GLM r31) | `TestActionResource_DeleteIdempotent` | DONE |
| T20 | P3 | Luki testowe GLM r29: awaria `hostinterface.delete` w update (ID zachowane, blad widoczny); env `ZABBIX_CA_CERT_FILE` przeplywa do klienta end-to-end | `TestHostUpdate_InterfaceDeleteFailureKeepsID`, `TestProviderConfigure_EnvCACertFile` | DONE |
| T19 | P3 | `CheckDestroy` obejmuje takze zasoby POMOCNICZE (grupy g/g2 w lifecycle hosta, grupa w tescie bez interfejsu i w sciezce Bearer) - pozorny sukces destroy nie kumuluje smieci w akceptacyjnym Zabbixie (GLM r26) | `TestAccHost_lifecycle`, `TestAccHost_noInterface`, `TestAccProvider_APIToken` | DONE |
| T18 | P2 | Payloady asertowane co do WARTOSCI, nie ksztaltu: pelny action.create/update (warunki z value2, opmessage z subject/message, konkretne ID odbiorcow), host.update (hostid/host/name/grupy/szablony/templates_clear po ID), parametry webhooka verbatim z sekretem; sciezka Bearer sprawdza destroy OBU zasobow (Codex r29) | `TestActionParams_EventSourceOnlyOnCreate`, `TestUpdateHost_NoInterfacesAndTemplatesClear`, `TestMediaTypeParams_TypeAware`, `TestAccProvider_APIToken` | DONE |
| T16 | P2 | Testy krytyczne zasilone PELNYM stanem sprzed zmiany: partial-state po nieudanej mutacji utrzymuje kazda stara wartosc (State() != nil wymagane) dla hostgroup/action/media; restricted `mediatype.get` zachowuje wszystkie istniejace atrybuty (w tym haslo); acceptance re-login naprawde ROWNOLEGLY z licznikiem `user.login` na transporcie (dokladnie 1) (Codex r27) | `TestPartialStateOnFailedUpdates`, `TestMediaTypeRead_RefusesRestrictedResponse`, `TestAccProvider_SessionRelogin` | DONE |
| T14 | P3 | Testy wspolbieznosci re-loginu z barierami z TIMEOUTEM (regresja liczby zadan = czytelny blad, nie wiszace CI); test retry mutacji asertuje `Bearer tok1` przy pierwszym zadaniu i `tok2` przy retry (Codex r21) | `TestCall_ReloginOnceOnSessionExpiry`, `TestCall_FailedReloginIsSharedByWaiters`, `TestCall_MutationRetriedExactlyOnceAfterRelogin` | DONE |
| H18 | P1 | Bariera LLD takze dla GRUP hostow (`flags=4` = grupa z group prototype regulki discovery): Read odmawia, Update/Delete maja fail-closed preflight (brak pola flags = odmowa mutacji) - import lub `-refresh=false` nie pozwoli przemianowac ani usunac grupy nalezacej do discovery (Codex r34) | `TestHostGroupMutations_RefuseDiscoveredGroup` | DONE |
| H17 | P3 | Interfejs agenta usuniety zewnetrznie miedzy preflight a `hostinterface.update` = przyjazny blad "vanished" jak na pozostalych sciezkach (GLM r32) | `TestHostUpdate_InterfaceVanishedMidUpdate` | DONE |
| H16 | P3 | `validateDNS` ogranicza charset do `[A-Za-z0-9._-]` (literowka typu `db01,,x` odpada w planie); delete niepustej grupy hostow niesie wskazowke "grupa z hostami nie moze byc usunieta" (GLM r31) | przypadki DNS w `TestHostCustomizeDiff`, `TestHostGroupDelete_NonEmptyGroupHint` | DONE |
| H15 | P3 | Bariera LLD fail-closed takze na BRAK pola `flags` w mutacjach (Update/Delete odmawiaja, gdy odpowiedz nie niesie flags - posrednik nie moze otworzyc bariery przez wyciecie pola; Read pozostaje tolerancyjny); grupa usunieta zewnetrznie miedzy planem a apply = przyjazny blad "vanished" jak w pozostalych zasobach; `email_provider` round-trip w acceptance (GLM r30) | `TestHostMutations_RefuseDiscoveredHost` (wariant no-flags), `TestHostGroupUpdate_VanishedGroup`, `TestAccMediaType_email` | DONE |
| H14 | P3 | Bariera LLD (`flags != 0`) na KAZDEJ sciezce mutujacej, nie tylko w Read: preflight w Update i wlasny preflight w Delete (z `-refresh=false` Read nie biegnie przed apply/destroy; stan po v0.1 moglby zmutowac hosta discovery) - wspolny `discoveredHostError`; drugi delete konczy sie na preflightcie bez mutujacego RPC (Codex r30) | `TestHostMutations_RefuseDiscoveredHost`, `TestHostResource_DeleteIdempotent` | DONE |
| H13 | P3 | Guardy `HasAttribute` przed kazdym `GetAttr` na raw config (czesciowe obiekty harnessa nie panikuja); walidacja `parameter.name` (pusta WARTOSC parametru celowo dozwolona); idempotentne usuwanie interfejsu agenta (rownolegla automatyzacja miedzy preflightem a delete); pelny lifecycle create/update/import/destroy na sciezce Bearer w acceptance; `go vet` takze w jobie Windows (GLM r21) | rozszerzone `TestWrittenInRaw` i `TestAccProvider_APIToken` | DONE |
| H12 | P2 | `expandHost` wyprowadza nieskonfigurowane `name` z ROZWIAZANEGO `host` (normalizacja CustomizeDiff pomijana przy unknown; stara nazwa ze stanu nie przezywa rename) (Codex r20); elementy setow `groups`/`templates`/`user_groups`/`users` walidowane na niepustosc; format time period (typ 6) walidowany w planie i na wartosciach rozwiazanych; asercje partial-state dla wszystkich 4 zasobow; dependabot dla akcji pinowanych po SHA; acceptance: pelny `providerConfigure` przez TLS proxy i zywy re-login po `user.logout` (GLM r20) | `TestExpandHost_NameFollowsResolvedHost`, `TestPartialStateOnFailedUpdates`, `TestAccProvider_SessionRelogin` | DONE |
| H11 | P3 | Zmiana technicznej nazwy `host` przez update (bez ForceNew, ID zachowane); przyklady importu bez hardcodowanych ID produkcyjnych + ostrzezenie o autorytatywnym imporcie grupy (GLM+Codex r15) | krok rename w `TestAccHost_lifecycle`, `examples/resources/*/import.sh` | DONE |
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
| M16 | P3 | Sonda restricted-response odporna na hipotetyczne pominiecie pol obcych typowi (dowolne z `smtp_server`/`description`/`maxsessions` = pelna odpowiedz); `max_sessions = 0` przechodzi przez realne API (GLM r17) | `TestMediaTypeRead_RefusesRestrictedResponse`, krok webhook w `TestAccMediaType_webhook` | DONE |
| M19 | P2 | Pole `provider` media typu Email (preset SMTP: 0 generic, 1 Gmail, 2 Gmail relay, 3 Office365, 4 O365 relay; 6.4+) zamodelowane jako `email_provider` (nazwa `provider` jest zarezerwowana w Terraformie) - pelny round-trip, reset przy zmianie typu, odmowa na innych typach; dziura w deklaracji M13 domknieta (GLM r29) | `TestMediaTypeParams_TypeAware`, `TestMediaTypeRead_EmailWithAuth`, `TestMediaTypeCustomizeDiff` | DONE |
| M18 | P3 | Read webhooka fail-closed na `timeout` spoza 1-60s (`d.Set` omija walidatory - wartosc z API zatrulaby stan); asercje payloadu update email rozszerzone o pola TLS/auth (takze w acceptance); test anulowanego inicjatora asertuje DOKLADNIE 2 loginy; README opisuje dwa wyjatki bootstrapu niosace sekret w body (`user.login`, `user.checkAuthentication`) i badge CI wskazuje fork publikujacy (Codex r28) | `TestMediaTypeRead_RefusesOutOfRangeWebhookTimeout`, `TestCall_ReloginSurvivesInitiatorCancellation`, `TestMediaTypeResource_EmailUpdatePayload` | DONE |
| M17 | P1 | Preflight update media type odmawia dokladnie tego co Read (wspolny `unmanageableMediaType`): parametry dodane do typu Script miedzy planem a apply blokuja mutacje PRZED zmiana zdalnego obiektu (Codex r20) | `TestMediaTypeUpdate_RefusesExternallyGainedScriptParameters` | DONE |
| M15 | P2 | `mediatype.get` fail-closed: okrojona odpowiedz (nie-Super-Admin od 6.4.19 dostaje tylko mediatypeid/name/type/status/maxattempts) = twardy blad zamiast wyzerowania konfiguracji; wymog Super Admina w opisie zasobu (Codex r16) | `TestMediaTypeRead_RefusesRestrictedResponse` | DONE |
| M14 | P3 | Sciezka update email pokryta jednostkowo (asercja payloadu) i akceptacyjnie (zmiana portu i hasla bez zmiany typu) (GLM r15) | `TestMediaTypeResource_EmailUpdatePayload`, krok update w `TestAccMediaType_email` | DONE |
| M13 | P2 | Pelny model media type 6.4 (Codex, r12): `description`, `max_sessions` (SMS wymusza 1), `max_attempts`, `attempt_interval` (0-1h, walidacja w planie), `content_type` (Email), `process_tags`, `show_event_menu` + `event_menu_url`/`event_menu_name` (Webhook, wymagane razem); Read resetuje pola obcych typow do defaultow SCHEMATU (jedno zrodlo, GLM r12) | `TestMediaTypeCustomizeDiff`, `TestMediaTypeRead_TypeAwareReset`, `TestMediaTypeParams_TypeAware`, `TestAccMediaType_webhook`, `TestAccMediaType_email` | DONE |

### 3.5 `zabbix_action`

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| A1 | P2 | `eventsource` ForceNew, tylko 0, nie wysylane w update | `TestActionCustomizeDiff`, C11 | DONE |
| A2 | P2 | `condition` jako `TypeSet`; expand z `*schema.Set` | `TestAccAction_lifecycle` (3 warunki w innej kolejnosci niz API, plan pusty) | DONE |
| A3 | P2 | `operationtype` tylko 0; `evaltype` w {0,1,2}; kazda operacja >= 1 odbiorca; `subject`/`message` tylko z `default_msg=false` (API odrzuca inaczej); `esc_step_to` 0 lub >= `esc_step_from`; `esc_period` 0 lub >= 60s | `TestActionCustomizeDiff`, `TestParseZabbixDuration` | DONE |
| A4 | P2 | `users` -> `opmessage_usr`; `opmessage_grp`/`opmessage_usr` ZAWSZE wysylane (rowniez puste) - Zabbix zachowuje starych odbiorcow gdy pole pominiete (potwierdzone testem) | krok 2 `TestAccAction_lifecycle` (grupy usuniete, user zostaje) | DONE |
| A5 | P3 | `pause_suppressed`, `notify_if_canceled` jako pola akcji (nie operacji) | round-trip w `TestAccAction_lifecycle` | DONE |
| A24 | P3 | `timePeriodRe` capuje koniec okresu na 24:00 (literowka 24:59 odpada w planie, nie w apply); nie-numeryczne wartosci w `flattenAction` odmawiaja ze wskazowka `terraform state rm` jak kazda inna nieobslugiwana forma (GLM r31) | przypadki time period w `TestActionCustomizeDiff`, `TestFlattenAction_NonNumericRefusesWithHint` | DONE |
| A23 | P1 | Unknown `eventsource` odrzucane juz w PLANIE: ForceNew + unknown planowaloby destrukcyjny replace, ktorego Create moze odrzucic PO skasowaniu akcji (Codex r26) | przypadek "unknown eventsource" w `TestActionCustomizeDiff` | DONE |
| A19 | P1 | Update akcji czyta biezacy obiekt i odmawia nadpisania ksztaltow spoza modelu dodanych po ostatnim refresh (recovery/update operations, opconditions) - `action.update` zastepuje operacje w calosci (Codex r17) | `TestActionUpdate_RefusesExternalUnmanagedShapes` | DONE |
| A20 | P2 | Rownowazne zapisy czasu (`3600` vs `1h`) nie generuja wiecznego diffa: DiffSuppress porownuje sparsowane sekundy dla `esc_period` (akcja i operacja), `attempt_interval` i `timeout`; makra nigdy nie sa wygaszane; wartosc niekanoniczna przechodzi przez acceptance (auto-check pustego planu po apply) (GLM r19) | `TestSuppressEquivalentDuration`, `esc_period = "1800"` w `TestAccAction_lifecycle` | DONE |
| A21 | P3 | Operacja bez `opmessage` = odmowa (refuse-not-guess, wczesniej cichy fallback "default message"); warning wersji wymienia wymog 6.4+ dla akcji (`pause_symptoms` zawsze wysylane); acceptance zalezne takze od unit-windows; `name` w liscie atrybutow importu hosta; testy: limit 32 MiB, override `timeouts` w acc (GLM r19) | `TestActionRead_RefusesOperationWithoutOpMessage`, `TestCall_OversizedResponseFails` | DONE |
| A18 | P3 | Walidacja w planie: `condition.value` niepuste/nie-bialy-znak, severity (typ 4) w 0-5 (uwaga: GLM podal 0-7, poprawne jest 0-5); `dns` hosta walidowane (bez spacji, <=255, makro OK); update hosta usunietego zewnetrznie = czytelny blad z zachowanym ID (GLM r17) | `TestActionCustomizeDiff`, `TestHostCustomizeDiff`, `TestHostUpdate_VanishedHost` | DONE |
| A17 | P2 | Akcje z `recovery_operations`/`update_operations` odmawiane w Read (selecty tylko do wykrycia; wczesniej cicho niezarzadzane po imporcie - naruszenie wzorca odmowy) (GLM r16) | `TestActionRead_RefusesRecoveryAndUpdateOperations` | DONE |
| A16 | P2 | `pause_symptoms` (6.4, tylko trigger actions) modelowane i round-tripowane; zmiana poza TF = drift (Codex r14) | `TestActionRead_Mapping`, krok opsUsersOnly w `TestAccAction_lifecycle` | DONE |
| A15 | P3 | `subject`/`message` przy `default_msg=false` wysylane ZAWSZE (takze puste) - merge semantics `action.update` wskrzeszaloby stara wartosc (GLM r13); przy `default_msg=true` niewysylane (API odrzuca). Makra uzytkownika przechodza przez realne API: `port` hosta i `esc_period` operacji (kroki akceptacyjne); delete hosta pokryty jednostkowo | `TestActionParams_CustomSubjectAlwaysSent`, kroki "cleared"/"macro" w `TestAccAction_lifecycle` i `TestAccHost_lifecycle`, `TestHostResource_DeleteIdempotent` | DONE |
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
| P1 | P2 | Walidacja URL (http/https); warning przy `http://` poza loopback; warning przy wersji spoza wspieranych linii (6.4.x/7.0.x) | `TestProviderConfigure_AuthValidation`, `TestProviderConfigure_WarnsOnPlainHTTPAndVersion` | DONE |
| P2 | P3 | `version` z ldflags -> `User-Agent` | `main.go`, `rawCall` | DONE |
| P4 | P2 | Minimalna wersja: 6.4.0 odrzucane w configure (brak `token` w `user.checkAuthentication` przed 6.4.1, ZBXNEXT-8012); inne linie = warning | `TestProviderConfigure_RejectsZabbix640`, `TestPlainHTTPWarning` | DONE |
| P5 | P3 | Zrodlo credentiali (HCL vs env) rozstrzygane po `GetRawConfig` (fallback: porownanie z env w harnessie testowym), nie po rownosci wartosci; warningi towarzysza bledom configure (operator widzi np. warning o plain HTTP takze gdy configure pada); sciezki bledow configure (apiinfo.version, user.login) pokryte testami | `TestWrittenInRaw`, `TestProviderConfigure_ConfigureErrorPaths`, przypadek "remote http warns" w `TestProviderConfigure_AuthValidation` | DONE |
| P6 | P1 | Update w trybie partial: SDKv2 zapisuje planowane wartosci do stanu mimo bledu update (udokumentowane w `ResourceData.Partial`) - poprzedni stan zachowany do potwierdzenia mutacji, `Partial(false)` przed koncowym Read (Codex r13) | asercja stanu w `TestHostUpdate_PartialFailureKeepsID` | DONE |
| P22 | P1 | Macierz goreleasera bez `windows/arm` (target usuniety w Go 1.25 - snapshot i release padalyby na cross-kompilacji zanim powstanie artefakt); test puli CA porownuje ODTWORZONA oczekiwana pule (system + wlasny cert) przez `Equal` (Codex r24) | lokalny snapshot goreleasera, `TestNewZabbixClient_TLS` | DONE |
| P21 | P3 | Domkniecia GLM r24: zdublowane (martwe) `Partial(true)` w update hosta - pierwsza proba usuniecia (r24) zostala pominieta przez guard skryptu i duplikat przezyl do r29, usuniety faktycznie w r29 (wykryte przez GLM r26); lokalna instrukcja akceptacji w README z krokami z CI (wait na `server #0 started` + `ANALYZE`, plus overlay 7.0); komentarz joba `ci` w release.yml zgodny z macierza. Do 10/10 GLM zada faktycznego release v0.2.0 (tag przez pipeline z GPG i publikacja w Registry) - akcja wlasciciela repo, poza petla recenzji | README, release.yml | DONE |
| P25 | P3 | Domkniecia GLM r28: `url` Optional+DefaultFunc (przyjazny blad o `ZABBIX_URL` osiagalny - Required odpalalby wczesniej surowe "Missing required argument"); notki wydania wyciagane z sekcji taga w CHANGELOG.md do body release (Registry je pokazuje); SKILL.md przepisany na stan v0.2 (stary opisywal repo sprzed hardeningu ze sciezkami /home/adi); link do Terraform Registry w README jako widoczny dowod publikacji | release.yml, README, `.agents/skills/` | DONE |
| P24 | P3 | Symetria walidacji rozwiazanych wartosci domknieta: `esc_period` akcji i operacji oraz `esc_step_from >= 1` w `validateActionValues`; format `ip`/`dns`/`port` powtorzony w `validateHostAddress` (blad przed mutacja, nie w polowie apply); ograniczenie operatorow 8/9 (matches, 7.0+) jawnie udokumentowane w README - macierz warunkow celowo 6.4-baseline z refuse-not-guess (GLM r27) | rozszerzone `TestActionApplyValidation_RejectsResolvedConflicts` i `TestHostApplyValidation_RejectsInvalidResolvedValues` | DONE |
| P20 | P2 | Rozwiazane enumy walidowane KOMPLETNIE przed mutacja: `type` media (spoza 0/1/2/4), `evaltype`, `conditiontype` (symetrycznie do eventsource/operationtype); TLS proxy w acceptance bez zdublowanej sciezki (SingleHostReverseProxy skleja target.Path z request.Path) + asercja odebranej sciezki; transportowy test GetBody==nil na zadaniu z rawCall (Codex r23) | rozszerzone testy apply-validation, `TestRawCall_TransportSeesNoGetBody` | DONE |
| P19 | P3 | `operationtype` sprawdzane takze na wartosci ROZWIAZANEJ (symetria z eventsource - unknown nie moze utworzyc akcji, ktorej Read od razu odmowi); usunieta martwa stala `SupportedVersionPrefix` (jedno zrodlo prawdy: `SupportedVersionPrefixes`) (GLM r23) | rozszerzony `TestActionApplyValidation_RejectsResolvedConflicts` | DONE |
| P18 | P3 | `eventsource` sprawdzane takze na wartosci ROZWIAZANEJ (unknown w planie nie wysle nietriggerowej akcji do API); opisy retry doprecyzowane: single-shot dotyczy bledow transportu, jedyny wyjatek to pojedyncze powtorzenie zadania odrzuconego przez wygasla sesje (Codex r22) | rozszerzony `TestActionApplyValidation_RejectsResolvedConflicts`, README i CHANGELOG | DONE |
| P17 | P2 | Macierz akceptacji CI: Zabbix 6.4.21 i 7.0.30 LTS (overlay digest-pinned); `SupportedVersionPrefixes` = {6.4, 7.0} - 7.0.x bez warningu, gate 6.4.1 bez zmian; jawny blad przy braku `url`; `validateDNS` liczy znaki (IDN), nie bajty; proxy z env (HTTPS_PROXY) udokumentowane w README (GLM r22) | matrix w ci.yml, `TestPlainHTTPWarning` (7.0.30/7.2.3), przypadek "missing url" | DONE |
| T15 | P3 | Acceptance: zmiana typu email->webhook z asercja wyczyszczonych credentiali PO STRONIE API; `ip`+`dns` jednoczesnie + przelaczenie `use_ip`; unit: udany update z padajacym finalnym Read (blad widoczny, ID zachowane; zapis planned values do nastepnego refresh to wlasciwosc SDKv2) (GLM r22) | kroki w `TestAccMediaType_email` i `TestAccHost_lifecycle`, `TestHostUpdate_FailedFinalReadSurfacesError` | DONE |
| P16 | P2 | Domkniecie walidacji rozwiazanych wartosci: `event_menu_url`/`event_menu_name` przy `show_event_menu` rozwiazanym do false = blad; pusty token z `user.login` odrzucany (zadne zadanie nie wychodzi cicho bez naglowka Authorization); precheck acceptance odrzuca komplet obu metod auth w srodowisku (Codex r19) | `TestCall_EmptyLoginTokenRejected`, przypadek webhook w `TestMediaTypeApplyValidation_RejectsResolvedConflicts` | DONE |
| P15 | P1 | `Partial(true)` od PIERWSZEJ instrukcji kazdego Update (blad walidacji lub preflightu przed mutacja tez nie moze zapisac planowanych wartosci); preflight update akcji przez pelny `flattenAction` (Update odmawia dokladnie tego co Read); `ClearParameters` liczone z AKTUALNEGO typu w API, nie z planu; pola obcego typu media sprawdzane ponownie po rozwiazaniu `type` (raw config); warningi transportowe zbierane przed wczesnymi bledami configure (Codex r18) | `TestActionUpdate_RefusesExternalUnmanagedShapes`, `TestMediaTypeUpdate_ClearsParametersOnExternalTypeDrift`, `TestForeignMediaTypeFields_ResolvedType`, przypadki "no credentials remote http" i "tls_insecure with ca_cert_file" | DONE |
| P12 | P1 | Walidacje cross-field powtorzone na wartosciach ROZWIAZANYCH w Create/Update (CustomizeDiff musi pomijac unknown): adres hosta (pole skonfigurowane a puste = blad, nie cichy host bez interfejsu), operacje/warunki akcji, wymagania per-typ media type z parowaniem credentiali (Codex r17) | `TestHostApplyValidation_RejectsInvalidResolvedValues`, `TestActionApplyValidation_RejectsResolvedConflicts`, `TestMediaTypeApplyValidation_RejectsResolvedConflicts` | DONE |
| P13 | P2 | Niejednoznaczny wynik create (blad transportu, nie JSON-RPC) = diagnostyka "outcome unknown" z instrukcja importu; release.yml: read-only preflight ancestry PRZED CI i approvalem environmentu, `permissions: contents: read` na workflow i `write` tylko na jobie goreleaser, granica zaufania (workflow wykonywany z commita taga) udokumentowana; samotne jawne `password` obok tokena = konflikt (Codex r17) | `TestHostGroupCreate_AmbiguousOutcomeHint`, przypadek "token with stray password" | DONE |
| P11 | P3 | Akceptacyjna sciezka TLS: reverse proxy z self-signed certem przed realnym API - `ca_cert_file` honorowany end-to-end, brak CA = odrzucenie handshake (GLM r17) | `TestAccProvider_TLSTerminatedProxy` | DONE |
| P10 | P3 | Jawnosc credentiali liczona takze po `password` (jawne haslo obok jawnego tokena = konflikt, nie ciche preferowanie tokena); `testAccCheckGone` wymaga obecnosci adresu w stanie; testy naprawy driftu zewnetrznego (interfejs hosta przez `hostinterface.update`, filtr akcji przez `action.update`) (Codex r16) | `TestProviderConfigure_ExplicitPasswordConflictsWithExplicitToken`, kroki drift w `TestAccHost_lifecycle` i `TestAccAction_lifecycle` | DONE |
| P9 | P3 | Checklist konfiguracji release w README (environment `release` + required reviewers + sekrety environmentowe); job unit na windows-latest (regresje sciezek/CRLF/TLS store; -race zostaje na Linux) (GLM r16) | README "Release", `.github/workflows/ci.yml` job `unit-windows` | DONE |
| P8 | P2 | Bramka wersji fail-closed: nienumeryczny patch 6.4 (rc/beta) = warning "untested", nigdy cichy sukces; testy obu galezi konfliktu auth (dwie jawne i dwie ambient); `timeout-minutes: 30` na jobie release; walidacja przykladow w CI tworzy katalog przed buildem (GLM+Codex r14) | `TestPlainHTTPWarning`, przypadek "both auth methods explicit", `TestProviderConfigure_BothAmbientMethodsConflict` | DONE |
| P7 | P2 | Pusty wynik Read bezposrednio po Create = blad z zachowanym ID (nie warning z osieroceniem swiezego obiektu); po rozstrzygnieciu zrodla auth ponowna walidacja kompletnosci credentiali | `TestHostGroupCreate_EmptyReadKeepsID`, `TestProviderConfigure_IncompleteCredentialsWithEnvToken` | DONE |

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
| T13 | P2 | Testy jednostkowe CRUD na poziomie zasobow (nie tylko klienta): host_group create/update/delete (z asercja payloadu update), action update z czyszczeniem odbiorcow, media type delete idempotentny, mutacja ponawiana dokladnie raz po re-loginie, czesciowa awaria update'u hosta zachowuje ID | `TestHostGroupResource_CRUD`, `TestActionResource_UpdateSendsClearedRecipients`, `TestMediaTypeResource_DeleteIdempotent`, `TestCall_MutationRetriedExactlyOnceAfterRelogin`, `TestHostUpdate_PartialFailureKeepsID` | DONE |
| T14 | P3 | CI: `staticcheck` (pinowany, v0.8.1), `concurrency` z `cancel-in-progress`, tygodniowy `schedule` + `workflow_dispatch` (swieze CVE bez commita), pelny snapshot release (`--skip=publish,sign` - archiwa, checksumy, manifest); template docs opisuje wykluczajace sie atrybuty, zastrzezenie o uprawnieniach i deklaracje zmiennych przykladu; acceptance asertuje niezmiennosc interfejsu SNMP; przyklad Slack sprawdza status HTTP | `ci.yml`, `templates/index.md.tmpl`, `acc_test.go` | DONE |
| T12 | P3 | Testy: domyslne timeouty 2 min asertowane dla 4 zasobow; `filter.conditions` zawsze tablica; sekret z URL nigdy w zadnej diagnostyce; `pause_suppressed`/`notify_if_canceled` = false round-trip w akceptacji | `TestResourceTimeoutsDefaults`, `TestActionParams_EventSourceOnlyOnCreate`, `TestProviderConfigure_AuthValidation`, `TestAccAction_lifecycle` | DONE |
| T11 | P2 | CI acceptance: `ANALYZE` swiezej bazy przed testami - bez statystyk planera zapytania `templates_clear` trwaly minuty (potwierdzone `pg_stat_activity`), hosty testowe tworzone jako wylaczone | `ci.yml`, run 33327064286 zielony | DONE |
| T9 | P2 | CI egzekwuje C7: blokujacy grep na `os.Stderr/Stdout`, `fmt.Fprint/Print`, `log.` w kodzie providera; `example_deployment/` w `terraform fmt -check` | `ci.yml` | DONE |
| T4 | P3 | `docker-compose.acc.yml` (upstream ignoruje `docker-compose.yml` jako plik lokalny): bez `version:`, obrazy `alpine-6.4.21`, healthchecki, porty na 127.0.0.1 | `docker compose -f docker-compose.acc.yml config` | DONE |

### 3.8 Dokumentacja

| ID | P | Wymaganie | AC | Status |
|---|---|---|---|---|
| D1 | P2 | `docs/` przez `go generate` (tfplugindocs), `examples/` z `resource.tf` + `import.sh`; CI sprawdza drift | `docs/resources/*.md` x4 + `docs/index.md` | DONE |
| D2 | P3 | README: 4 zasoby, `api_token`, TLS, zachowania (Read/interfejsy/templates_clear), sekcja o obiektach nieobslugiwanych (`terraform state rm`), akceptacja, override dla Windows, CHANGELOG | `README.md`, `CHANGELOG.md` | DONE |
| D3 | P2 | Opisy zasobow (i wygenerowane docs) ostrzegaja o autorytatywnym zarzadzaniu przy imporcie: WSZYSTKIE atrybuty (rowniez defaulty typu `enabled`, `esc_period`) trzeba odtworzyc i przejrzec plan przed pierwszym apply; przyklady bez hardcodowanych ID instancji (zmienne) | `docs/resources/*.md`, `examples/` | DONE |
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
- `user.checkAuthentication` przyjmuje parametr `token` dopiero od 6.4.1 (ZBXNEXT-8012);
  provider odrzuca 6.4.0 w configure z jasna diagnostyka (Codex, r12).
- ZBX-22952: proxy/webserver gubiacy naglowek `Authorization` daje "Not authorized" przy CRUD
  mimo poprawnego configure (provider uzywa wylacznie Bearer) - udokumentowane w README.

## 4a. Uwagi recenzentow odrzucone (z uzasadnieniem)

- (Codex, r7) "Redagowac `message`/`data` bledow JSON-RPC w diagnostyce": tresc bledu API to
  podstawowa diagnostyka providera (kazdy provider Terraform ja pokazuje); koperta odpowiedzi
  jest walidowana (C21), a body HTTP nie-JSON-RPC nigdy nie trafia do bledow (C18).
- (Codex, r5) "Wymusic HTTPS": warning przy nie-loopbackowym http (P1) zamiast twardej blokady -
  swiadomy wybor, lokalne laby i test acceptance chodza po http.
- (GLM, r7) "Wspoldzielona mapa schematu media type": jedna instancja providera na proces,
  SDK nie mutuje schematu po InternalValidate; przebudowa per wywolanie bez zysku.
- (Codex, r24) "Odrzucac smtp_authentication=1 przy smtp_security=0": Zabbix dopuszcza te
  kombinacje (typowy przyklad: lokalny relay 127.0.0.1:25), a twardy blad uniemozliwilby
  zarzadzanie legalnie zaimportowanymi obiektami (konfiguracja musi odwzorowac stan).
  Ryzyko opisane w dokumentacji `smtp_security` - swiadomie poziom dokumentacji/warningu,
  analogicznie do plain HTTP w providerze.
- (Codex, r12) "Destroy przy utracie uprawnien konczy sie pozornym sukcesem": API Zabbix
  zwraca ten sam blad dla brakujacego obiektu i braku uprawnien, a Get pusty wynik
  (fakty w sekcji 4) - rozroznienie jest niemozliwe. Konwencja providerow Terraform:
  potwierdzona niewidocznosc = usuniete. Udokumentowane w docs/index i README
  ("The same applies to terraform destroy").
- (GLM, r32) "SchemaVersion=1 + StateUpgraders dla migracji stanu v0.1->v0.2": zaden stan
  v0.1 nie istnieje w obiegu - v0.1.0 upstreamu nigdy nie zostal opublikowany (release
  padl bez artefaktow, Registry serwuje wylacznie 0.2.x), wiec upgrader migrowalby stan,
  ktorego nikt nie ma; nota migracyjna w CHANGELOG pokrywa hipotetyczne buildy ze zrodel.
  StateUpgraders wejda przy pierwszej realnej zmianie schematu po 0.2.
- (GLM, r32) "Wspolna metatabela regul walidacji (CustomizeDiff/Create/preflight)":
  odroczone do v0.3 (sekcja 2) - trzy miejsca sa zsynchronizowane i przypiete testami
  wartosci; przebudowa na generowana tabele tuz przed wydaniem niesie wieksze ryzyko
  regresji niz utrzymanie konwencji.
- (GLM, r10) "Import ID szablonu jako hosta uszkadza szablon": obalone empirycznie -
  `host.get` z ID szablonu zwraca pusta liste na 6.4.21 (szablony nie sa hostami w API);
  dodatkowo Read odmawia hostow z LLD (`flags != 0`).

## 5. Ryzyka pozostale

- Utrata uprawnien = "not found" (patrz 3.2).
- `TestAccProvider_APIToken` wymaga poswiadczen haslem (mintuje token); przy uruchomieniu
  akceptacji samym tokenem jest pomijany.
- `zabbix_host` zarzadza tylko glownym interfejsem agenta (tworzy go, gdy brak); pozostale interfejsy sa nietykane;
  v0.3: blok `interface` (lista).
- Zabbix 6.4 jest EOL; od r22 akceptacja CI obejmuje takze 7.0 LTS (matrix,
  `docker-compose.acc-70.yml`); linie inne niz 6.4/7.0 = warning "untested".
  Pelny pakiet TestAcc potwierdzony empirycznie na 7.0.30 lokalnie (62 s, zielony).
- Sciezka restricted `mediatype.get` pokryta fixturem jednostkowym; wariant akceptacyjny
  z realnym uzytkownikiem nie-Super-Admin oraz akceptacja na 7.0 LTS = zakres v0.3.
- `esc_period` z makrem uzytkownika nie jest walidowane (nie da sie).
- Namespace: Terraform `source`, dokumentacja, przyklady i override CI = `SychPL/zabbix`
  (repo publikujace; bramka CI pilnuje stalych odwolan). `Tensai123` zostaje WYLACZNIE
  w go-modulowym module path (kod, niewidoczny dla uzytkownikow Terraforma) i jako upstream.
- Environment `release` wymaga skonfigurowania required reviewers w ustawieniach repo, zeby faktycznie
  bramkowal uzycie klucza GPG.
