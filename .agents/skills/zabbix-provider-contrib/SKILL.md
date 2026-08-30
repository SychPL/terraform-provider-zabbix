---
name: zabbix-provider-contrib
description: Przewodnik i instrukcje wdrażania zmian, dodawania nowych zasobów oraz testowania Terraform Providera dla Zabbix.
---

# Zabbix Terraform Provider — Instrukcja Współpracy (Contribution Guide)

Witaj! Niniejszy przewodnik opisuje strukturę projektu, architekturę kodu oraz kroki niezbędne do rozwijania i dodawania nowych zasobów w Terraform Providerze dla systemu Zabbix.

---

## 🛠️ Wymagania Wstępne (Prerequisites)

1. **Środowisko Go:**
   Projekt kompiluje się przy użyciu **Go 1.22.6+** zlokalizowanego lokalnie pod ścieżką `/home/adi/go/bin/go`.
   Aby wykonywać polecenia kompilacji i testowania, zawsze używaj pełnej ścieżki lub dodaj ją do zmiennej PATH.

2. **Wersja serwera Zabbix:**
   Ten provider został zaprojektowany z myślą o współpracy z **Zabbix API 6.4**.
   * Korzysta on z metody `apiinfo.version` do dynamicznego badania kompatybilności.
   * Upewnij się, że dodając nowe parametry, są one zgodne ze specyfikacją Zabbix 6.4 API (np. interfejsy i pobieranie szablonów).

---

## 📂 Architektura Kodu Źródłowego

* [main.go](file:///home/adi/terraform-provider-zabbix/main.go) – Punkt wejściowy wtyczki, uruchamiający serwer RPC dla Terraforma.
* [zabbix/client.go](file:///home/adi/terraform-provider-zabbix/zabbix/client.go) – Autorski klient JSON-RPC 2.0 do komunikacji z API Zabbixa (obsługuje sesje, pobieranie wersji oraz wywołania CRUD).
* [zabbix/provider.go](file:///home/adi/terraform-provider-zabbix/zabbix/provider.go) – Schemat konfiguracji wejściowej providera (URL, Login, Hasło) i rejestracja zasobów.
* [zabbix/resource_host_group.go](file:///home/adi/terraform-provider-zabbix/zabbix/resource_host_group.go) – Logika obsługi cyklu życia zasobu `zabbix_host_group`.
* [zabbix/resource_host.go](file:///home/adi/terraform-provider-zabbix/zabbix/resource_host.go) – Logika obsługi cyklu życia zasobu `zabbix_host`.

---

## 📝 Instrukcja Krok po Kroku: Dodawanie Nowego Zasobu

Aby dodać nowy zasób (np. `zabbix_template` lub `zabbix_user`):

### 1. Rozszerzenie Klienta API
W pliku [client.go](file:///home/adi/terraform-provider-zabbix/zabbix/client.go) zdefiniuj struktury danych oraz metody pomocnicze do obsługi CRUD. Przykładowo, dla grupy użytkowników:
```go
type UserGroup struct {
	Usrgrpid string `json:"usrgrpid"`
	Name     string `json:"name"`
}

func (c *ZabbixClient) CreateUserGroup(name string) (string, error) {
	// Wywołanie metody RPC "usergroup.create"
}
```

### 2. Utworzenie Pliku Kontrolera Zasobu
Stwórz nowy plik o nazwie `zabbix/resource_<nazwa>.go` (np. `zabbix/resource_user_group.go`). Zaimplementuj logikę CRUD opartą o szkielet Terraform SDKv2:
```go
package zabbix

import (
	"context"
	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

func resourceUserGroup() *schema.Resource {
	return &schema.Resource{
		CreateContext: resourceUserGroupCreate,
		ReadContext:   resourceUserGroupRead,
		UpdateContext: resourceUserGroupUpdate,
		DeleteContext: resourceUserGroupDelete,
		Schema: map[string]*schema.Schema{
			"name": {
				Type:     schema.TypeString,
				Required: true,
			},
		},
	}
}
```

### 3. Rejestracja Zasobu w Providerze
W pliku [provider.go](file:///home/adi/terraform-provider-zabbix/zabbix/provider.go) dodaj nowy zasób do mapy `ResourcesMap`:
```go
		ResourcesMap: map[string]*schema.Resource{
			"zabbix_host_group": resourceHostGroup(),
			"zabbix_host":       resourceHost(),
			"zabbix_user_group": resourceUserGroup(), // Dodaj tę linię
		},
```

### 4. Budowanie i Weryfikacja
Uruchom kompilację i uporządkuj zależności:
```bash
/home/adi/go/bin/go mod tidy
/home/adi/go/bin/go build -o terraform-provider-zabbix
```

---

## 🧪 Procedura Testowania (Acceptance Tests)

Aby przetestować integrację z działającym serwerem testowym Zabbix, ustaw następujące zmienne środowiskowe:
```bash
export ZABBIX_URL="http://twój-host-zabbix/zabbix/api_jsonrpc.php"
export ZABBIX_USERNAME="Admin"
export ZABBIX_PASSWORD="twojehaslo"
```

Następnie uruchom testy akceptacyjne:
```bash
/home/adi/go/bin/go test ./zabbix -v
```

---

## 🚀 Praca z Gitem i Pull Requesty

1. **Utwórz nową gałąź:**
   ```bash
   git checkout -b feature/nazwa-funkcji
   ```
2. **Dodaj i zatwierdź zmiany:**
   Pisz semantyczne opisy commitów (np. `feat: add zabbix_user_group resource`):
   ```bash
   git add .
   git commit -m "feat: add zabbix_user_group resource"
   ```
3. **Wypchnij zmiany na GitHub:**
   ```bash
   git push origin feature/nazwa-funkcji
   ```
