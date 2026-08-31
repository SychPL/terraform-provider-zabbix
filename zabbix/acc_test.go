package zabbix

import (
	"bytes"
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/acctest"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/resource"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/hashicorp/terraform-plugin-sdk/v2/terraform"
)

// Acceptance tests run against a real Zabbix (see docker-compose.acc.yml):
//
//	TF_ACC=1 ZABBIX_URL=http://localhost:8082/api_jsonrpc.php ZABBIX_USERNAME=Admin ZABBIX_PASSWORD=zabbix go test ./zabbix -run TestAcc -count=1 -v

var testAccProviderFactories = map[string]func() (*schema.Provider, error){
	"zabbix": func() (*schema.Provider, error) { return Provider(), nil },
}

func testAccPreCheck(t *testing.T) {
	t.Helper()
	if os.Getenv("TF_ACC") == "" {
		t.Skip("TF_ACC not set; skipping acceptance test")
	}
	if os.Getenv("ZABBIX_URL") == "" {
		t.Fatal("ZABBIX_URL must be set for acceptance tests")
	}
	if os.Getenv("ZABBIX_API_TOKEN") == "" && (os.Getenv("ZABBIX_USERNAME") == "" || os.Getenv("ZABBIX_PASSWORD") == "") {
		t.Fatal("ZABBIX_API_TOKEN or ZABBIX_USERNAME+ZABBIX_PASSWORD must be set for acceptance tests")
	}
	if os.Getenv("ZABBIX_API_TOKEN") != "" && (os.Getenv("ZABBIX_USERNAME") != "" || os.Getenv("ZABBIX_PASSWORD") != "") {
		t.Fatal("set either ZABBIX_API_TOKEN or ZABBIX_USERNAME+ZABBIX_PASSWORD, not both: the provider rejects two ambient auth methods")
	}
	if want := os.Getenv("ZABBIX_ACC_EXPECT_VERSION"); want != "" {
		// The CI matrix asserts it really talks to the Zabbix line it names.
		v, err := testAccClient(t).GetVersion(context.Background())
		if err != nil {
			t.Fatalf("ZABBIX_ACC_EXPECT_VERSION is set but apiinfo.version failed: %v", err)
		}
		if v != want && !strings.HasPrefix(v, want+".") {
			t.Fatalf("connected Zabbix reports version %s, expected the %s line", v, want)
		}
	}
}

// accClientConfig builds the raw-API client config from the same environment
// the provider under test reads - including the TLS knobs, so the suite works
// against HTTPS with a private CA too.
func accClientConfig() ClientConfig {
	insecure, _ := strconv.ParseBool(os.Getenv("ZABBIX_TLS_INSECURE"))
	return ClientConfig{
		URL:        os.Getenv("ZABBIX_URL"),
		Username:   os.Getenv("ZABBIX_USERNAME"),
		Password:   os.Getenv("ZABBIX_PASSWORD"),
		APIToken:   os.Getenv("ZABBIX_API_TOKEN"),
		Insecure:   insecure,
		CACertFile: os.Getenv("ZABBIX_CA_CERT_FILE"),
	}
}

// testAccClient returns a raw API client for test setup and verification.
func testAccClient(t *testing.T) *ZabbixClient {
	t.Helper()
	c, err := NewZabbixClient(accClientConfig())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHelperClientTLSEnv(t *testing.T) {
	t.Setenv("ZABBIX_URL", "https://zabbix.internal/api_jsonrpc.php")
	t.Setenv("ZABBIX_TLS_INSECURE", "1")
	t.Setenv("ZABBIX_CA_CERT_FILE", "/etc/ssl/private-ca.pem")
	cfg := accClientConfig()
	if !cfg.Insecure || cfg.CACertFile != "/etc/ssl/private-ca.pem" {
		t.Fatalf("the acceptance helper client must honour the TLS environment, got %+v", cfg)
	}
}

// lookupID returns the first ID of an object matched by filter.
func lookupID(t *testing.T, method, idField string, filter map[string]interface{}) string {
	t.Helper()
	var res []map[string]interface{}
	err := testAccClient(t).Call(context.Background(), method, map[string]interface{}{"output": []string{idField}, "filter": filter}, &res)
	if err != nil || len(res) == 0 {
		t.Fatalf("%s %v: %v (%d results)", method, filter, err, len(res))
	}
	return res[0][idField].(string)
}

func testAccProviderConfig() string {
	return `provider "zabbix" {}` + "\n"
}

func stateID(s *terraform.State, addr string) (string, error) {
	rs, ok := s.RootModule().Resources[addr]
	if !ok {
		return "", fmt.Errorf("%s not in state", addr)
	}
	return rs.Primary.ID, nil
}

// --- zabbix_host_group ---

func TestAccHostGroup_basic(t *testing.T) {
	name := acctest.RandomWithPrefix("tfacc-group")
	cfg := func(n string) string {
		return testAccProviderConfig() + fmt.Sprintf(`resource "zabbix_host_group" "g" { name = %q }`, n)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_host_group.g", func(c *ZabbixClient, id string) error { _, err := c.GetHostGroup(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg(name), Check: resource.TestCheckResourceAttr("zabbix_host_group.g", "name", name)},
			{Config: cfg(name + "-renamed"), Check: resource.TestCheckResourceAttr("zabbix_host_group.g", "name", name+"-renamed")},
			{ResourceName: "zabbix_host_group.g", ImportState: true, ImportStateVerify: true},
		},
	})
}

// testAccCheckGone asserts the object no longer exists after destroy.
func testAccCheckGone(t *testing.T, addr string, get func(*ZabbixClient, string) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[addr]
		if !ok {
			// A resource that vanished from state mid-test would make the
			// destroy check pass vacuously while orphaning the object.
			return fmt.Errorf("%s not present in the final state (lost mid-test?)", addr)
		}
		if err := get(testAccClient(t), rs.Primary.ID); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("%s %s still exists (err=%v)", addr, rs.Primary.ID, err)
		}
		return nil
	}
}

// --- zabbix_host ---

func TestAccHost_lifecycle(t *testing.T) {
	testAccPreCheck(t)
	name := acctest.RandomWithPrefix("tfacc-host")
	// Small stock templates: linking large ones (hundreds of items) can take
	// minutes on a cold CI Zabbix and blocks the API for other tests.
	templateA := lookupID(t, "template.get", "templateid", map[string]interface{}{"host": "OS processes by Zabbix agent"})
	templateB := lookupID(t, "template.get", "templateid", map[string]interface{}{"host": "Systemd by Zabbix agent 2"})

	base := testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_host_group" "g" { name = "%s-grp" }
resource "zabbix_host_group" "g2" { name = "%s-grp2" }
`, name, name)
	// Test hosts are created disabled: an enabled host with an unroutable IP is
	// polled by the server, whose item_rtdata writes hold row locks that block
	// host.update/host.delete for minutes on a slow CI runner.
	cfgIP := base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host      = %q
  enabled   = false
  groups    = [zabbix_host_group.g.id]
  templates = [%q, %q]
  ip        = "192.0.2.10"

  timeouts {
    create = "10m"
  }
}`, name, templateA, templateB)
	cfgUpdated := base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host        = %q
  name        = "%s visible"
  enabled     = false
  description = "managed by terraform"
  groups      = [zabbix_host_group.g.id]
  templates   = [%q]
  ip          = "192.0.2.11"
  port        = "10051"
}`, name, name, templateA)
	cfgDNS := base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host    = %q
  enabled = false
  groups  = [zabbix_host_group.g.id, zabbix_host_group.g2.id]
  use_ip  = false
  dns     = "agent.example.test"
}`, name)

	var hostID string
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			testAccCheckGone(t, "zabbix_host.h", func(c *ZabbixClient, id string) error { _, err := c.GetHost(context.Background(), id); return err }),
			testAccCheckGone(t, "zabbix_host_group.g", func(c *ZabbixClient, id string) error { _, err := c.GetHostGroup(context.Background(), id); return err }),
			testAccCheckGone(t, "zabbix_host_group.g2", func(c *ZabbixClient, id string) error { _, err := c.GetHostGroup(context.Background(), id); return err }),
		),
		Steps: []resource.TestStep{
			{
				Config: cfgIP,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "name", name),
					resource.TestCheckResourceAttr("zabbix_host.h", "enabled", "false"),
					resource.TestCheckResourceAttr("zabbix_host.h", "templates.#", "2"),
					resource.TestCheckResourceAttr("zabbix_host.h", "port", "10050"),
					func(s *terraform.State) error { var err error; hostID, err = stateID(s, "zabbix_host.h"); return err },
				),
			},
			{
				// An SNMP interface added outside Terraform must survive an interface update.
				PreConfig: func() { testAccAddSNMPInterface(t, hostID) },
				Config:    cfgUpdated,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "name", name+" visible"),
					resource.TestCheckResourceAttr("zabbix_host.h", "enabled", "false"),
					resource.TestCheckResourceAttr("zabbix_host.h", "templates.#", "1"),
					resource.TestCheckResourceAttr("zabbix_host.h", "ip", "192.0.2.11"),
					resource.TestCheckResourceAttr("zabbix_host.h", "port", "10051"),
					testAccCheckHostInterfaces(t, "zabbix_host.h", 2),
				),
			},
			{
				// External drift of the managed agent interface must be repaired
				// by an apply of the unchanged configuration.
				PreConfig: func() {
					host, err := testAccClient(t).GetHost(context.Background(), hostID)
					if err != nil {
						t.Fatal(err)
					}
					iface := host.AgentInterface()
					if iface == nil {
						t.Fatal("expected an agent interface")
					}
					params := map[string]interface{}{"interfaceid": iface.InterfaceID, "port": "10099"}
					if err := testAccClient(t).Call(context.Background(), "hostinterface.update", params, nil); err != nil {
						t.Fatal(err)
					}
				},
				Config: cfgUpdated,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "port", "10051"),
					func(s *terraform.State) error {
						host, err := testAccClient(t).GetHost(context.Background(), hostID)
						if err != nil {
							return err
						}
						if iface := host.AgentInterface(); iface == nil || iface.Port != "10051" {
							return fmt.Errorf("external interface drift not repaired: %+v", host.Interfaces)
						}
						return nil
					},
				),
			},
			{
				Config: cfgDNS,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "name", name), // not configured -> follows host
					resource.TestCheckResourceAttr("zabbix_host.h", "use_ip", "false"),
					resource.TestCheckResourceAttr("zabbix_host.h", "dns", "agent.example.test"),
					resource.TestCheckResourceAttr("zabbix_host.h", "ip", ""),
					resource.TestCheckResourceAttr("zabbix_host.h", "groups.#", "2"),
					resource.TestCheckResourceAttr("zabbix_host.h", "templates.#", "0"),
					testAccCheckHostInterfaces(t, "zabbix_host.h", 2),
				),
			},
			{
				// Renaming the technical host name must update in place.
				Config: base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host    = "%s-renamed"
  enabled = false
  groups  = [zabbix_host_group.g.id]
  ip      = "192.0.2.12"
}`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "host", name+"-renamed"),
					resource.TestCheckResourceAttr("zabbix_host.h", "name", name+"-renamed"),
					func(s *terraform.State) error {
						id, err := stateID(s, "zabbix_host.h")
						if err != nil {
							return err
						}
						if id != hostID {
							return fmt.Errorf("technical rename must not recreate the host (was %s, now %s)", hostID, id)
						}
						return nil
					},
				),
			},
			{
				// ip and dns configured together: the unused address must be
				// preserved end to end.
				Config: base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host    = "%s-renamed"
  enabled = false
  groups  = [zabbix_host_group.g.id]
  ip      = "192.0.2.13"
  dns     = "agent.example.test"
  use_ip  = true
}`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "ip", "192.0.2.13"),
					resource.TestCheckResourceAttr("zabbix_host.h", "dns", "agent.example.test"),
					resource.TestCheckResourceAttr("zabbix_host.h", "use_ip", "true"),
				),
			},
			{
				// Flipping use_ip keeps both addresses.
				Config: base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host    = "%s-renamed"
  enabled = false
  groups  = [zabbix_host_group.g.id]
  ip      = "192.0.2.13"
  dns     = "agent.example.test"
  use_ip  = false
}`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "use_ip", "false"),
					resource.TestCheckResourceAttr("zabbix_host.h", "ip", "192.0.2.13"),
					resource.TestCheckResourceAttr("zabbix_host.h", "dns", "agent.example.test"),
				),
			},
			{
				// User macro as the agent port must round-trip through the API.
				Config: base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host    = %q
  enabled = false
  groups  = [zabbix_host_group.g.id]
  ip      = "192.0.2.12"
  port    = "{$TFACC.AGENT.PORT}"
}`, name),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "port", "{$TFACC.AGENT.PORT}"),
					resource.TestCheckResourceAttr("zabbix_host.h", "ip", "192.0.2.12"),
				),
			},
			{ResourceName: "zabbix_host.h", ImportState: true, ImportStateVerify: true},
		},
	})
}

func testAccAddSNMPInterface(t *testing.T, hostID string) {
	t.Helper()
	params := map[string]interface{}{
		"hostid": hostID, "type": 2, "main": 1, "useip": 1, "ip": "192.0.2.99", "dns": "", "port": "161",
		"details": map[string]interface{}{"version": 2, "community": "public"},
	}
	if err := testAccClient(t).Call(context.Background(), "hostinterface.create", params, nil); err != nil {
		t.Fatalf("hostinterface.create: %v", err)
	}
}

func testAccCheckHostInterfaces(t *testing.T, addr string, want int) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		id, err := stateID(s, addr)
		if err != nil {
			return err
		}
		host, err := testAccClient(t).GetHost(context.Background(), id)
		if err != nil {
			return err
		}
		if len(host.Interfaces) != want {
			return fmt.Errorf("host %s: want %d interfaces, got %d (%+v)", id, want, len(host.Interfaces), host.Interfaces)
		}
		// The unmanaged SNMP interface must survive untouched (same address) -
		// and must still exist when two interfaces are expected (a second agent
		// interface replacing it would keep the count at two).
		var snmp int
		for _, iface := range host.Interfaces {
			if iface.Type != "2" {
				continue
			}
			snmp++
			if iface.IP != "192.0.2.99" || iface.Port != "161" {
				return fmt.Errorf("host %s: the SNMP interface was modified: %+v", id, iface)
			}
		}
		if want == 2 && snmp != 1 {
			return fmt.Errorf("host %s: the unmanaged SNMP interface disappeared (interfaces: %+v)", id, host.Interfaces)
		}
		return nil
	}
}

func TestAccHost_noInterface(t *testing.T) {
	name := acctest.RandomWithPrefix("tfacc-noiface")
	base := testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_host_group" "g" { name = "%s-grp" }
`, name)
	bare := base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host    = %q
  enabled = false
  groups  = [zabbix_host_group.g.id]
}`, name)
	withIP := base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host    = %q
  enabled = false
  groups  = [zabbix_host_group.g.id]
  ip      = "192.0.2.30"
}`, name)
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			testAccCheckGone(t, "zabbix_host.h", func(c *ZabbixClient, id string) error { _, err := c.GetHost(context.Background(), id); return err }),
			testAccCheckGone(t, "zabbix_host_group.g", func(c *ZabbixClient, id string) error { _, err := c.GetHostGroup(context.Background(), id); return err }),
		),
		Steps: []resource.TestStep{
			{Config: bare, Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_host.h", "ip", ""),
				resource.TestCheckResourceAttr("zabbix_host.h", "dns", ""),
				testAccCheckHostInterfaces(t, "zabbix_host.h", 0),
			)},
			{Config: withIP, Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_host.h", "ip", "192.0.2.30"),
				testAccCheckHostInterfaces(t, "zabbix_host.h", 1),
			)},
			{Config: bare, Check: testAccCheckHostInterfaces(t, "zabbix_host.h", 0)},
			{ResourceName: "zabbix_host.h", ImportState: true, ImportStateVerify: true},
		},
	})
}

// --- zabbix_media_type ---

func TestAccMediaType_webhook(t *testing.T) {
	name := acctest.RandomWithPrefix("tfacc-webhook")
	cfg := func(n string, params string) string {
		return testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_media_type" "wh" {
  name   = %q
  type   = 4
  script = "return 'OK';"
%s}`, n, params)
	}
	twoParams := `
  max_sessions     = 0
  max_attempts     = 5
  attempt_interval = "30s"
  process_tags     = true
  show_event_menu  = true
  event_menu_url   = "https://example.test/{EVENT.ID}"
  event_menu_name  = "Details"

  parameter {
    name  = "url"
    value = "https://hooks.example.test/x"
  }
  parameter {
    name  = "token"
    value = "s3cret"
  }
`
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_media_type.wh", func(c *ZabbixClient, id string) error { _, err := c.GetMediaType(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg(name, ""), Check: func(s *terraform.State) error {
				var err error
				id, err = stateID(s, "zabbix_media_type.wh")
				return err
			}},
			// Rename without parameters used to fail with "parameters: an array is expected".
			{Config: cfg(name+"-renamed", ""), Check: resource.TestCheckResourceAttr("zabbix_media_type.wh", "name", name+"-renamed")},
			{Config: cfg(name, twoParams), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.wh", "parameter.#", "2"),
				resource.TestCheckResourceAttr("zabbix_media_type.wh", "max_attempts", "5"),
				resource.TestCheckResourceAttr("zabbix_media_type.wh", "max_sessions", "0"),
				resource.TestCheckResourceAttr("zabbix_media_type.wh", "process_tags", "true"),
				resource.TestCheckResourceAttr("zabbix_media_type.wh", "show_event_menu", "true"),
				resource.TestCheckResourceAttr("zabbix_media_type.wh", "event_menu_name", "Details"),
			)},
			{
				// Parameters removed outside Terraform must be detected and restored.
				PreConfig: func() {
					params := map[string]interface{}{"mediatypeid": id, "parameters": []interface{}{}}
					if err := testAccClient(t).Call(context.Background(), "mediatype.update", params, nil); err != nil {
						t.Fatal(err)
					}
				},
				Config: cfg(name, twoParams),
				Check: func(s *terraform.State) error {
					mt, err := testAccClient(t).GetMediaType(context.Background(), id)
					if err != nil {
						return err
					}
					if len(mt.Parameters) != 2 {
						return fmt.Errorf("want 2 parameters restored, got %d", len(mt.Parameters))
					}
					return nil
				},
			},
			{ResourceName: "zabbix_media_type.wh", ImportState: true, ImportStateVerify: true},
		},
	})
}

func TestAccMediaTypeImport_ExternallyCreated(t *testing.T) {
	// An object created outside Terraform (raw API, as from the UI) must
	// import and converge to an empty plan - this exercises the provider
	// against API-normalised values it did not write itself.
	name := acctest.RandomWithPrefix("tfacc-ext-import")
	cfg := testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_media_type" "ext" {
  name             = %q
  type             = 4
  script           = "return 'OK';"
  timeout          = "45s"
  max_attempts     = 4
  attempt_interval = "15s"

  parameter {
    name  = "url"
    value = "https://hooks.example.test/ext"
  }
}`, name)
	var id string
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_media_type.ext", func(c *ZabbixClient, id string) error { _, err := c.GetMediaType(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{
				PreConfig: func() {
					var res map[string][]string
					params := map[string]interface{}{
						"name": name, "type": "4", "status": "0", "script": "return 'OK';",
						"timeout": "45s", "maxattempts": "4", "attempt_interval": "15s",
						"parameters": []interface{}{map[string]string{"name": "url", "value": "https://hooks.example.test/ext"}},
					}
					if err := testAccClient(t).Call(context.Background(), "mediatype.create", params, &res); err != nil {
						t.Fatal(err)
					}
					if len(res["mediatypeids"]) != 1 {
						t.Fatalf("unexpected mediatype.create result %v", res)
					}
					id = res["mediatypeids"][0]
				},
				Config:             cfg,
				ResourceName:       "zabbix_media_type.ext",
				ImportState:        true,
				ImportStateIdFunc:  func(*terraform.State) (string, error) { return id, nil },
				ImportStatePersist: true,
			},
			// The imported state must already match the configuration.
			{Config: cfg, PlanOnly: true},
		},
	})
}

func TestAccMediaType_email(t *testing.T) {
	name := acctest.RandomWithPrefix("tfacc-email")
	cfg := func(port int, password string) string {
		return testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_media_type" "mail" {
  name                = %q
  type                = 0
  enabled             = false
  smtp_server         = "mail.example.test"
  smtp_port           = %d
  smtp_helo           = "example.test"
  smtp_email          = "zabbix@example.test"
  smtp_security       = 1
  smtp_verify_peer    = true
  smtp_authentication = 1
  username            = "zabbix"
  password            = %q
  content_type        = 0
  email_provider      = 3
  description         = "managed by terraform"
  max_attempts        = 5
}`, name, port, password)
	}
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_media_type.mail", func(c *ZabbixClient, id string) error { _, err := c.GetMediaType(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg(587, "hunter2"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "enabled", "false"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "smtp_port", "587"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "password", "hunter2"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "content_type", "0"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "email_provider", "3"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "max_attempts", "5"),
			)},
			// Update without a type change must round-trip the changed values.
			{Config: cfg(2525, "hunter3"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "smtp_port", "2525"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "password", "hunter3"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "smtp_verify_peer", "true"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "smtp_security", "1"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "smtp_authentication", "1"),
			)},
			// Switching away from email must clear the SMTP credentials in
			// Zabbix itself, not only in the Terraform state.
			{Config: testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_media_type" "mail" {
  name   = %q
  type   = 4
  script = "return 'OK';"
}`, name), Check: func(s *terraform.State) error {
				id, err := stateID(s, "zabbix_media_type.mail")
				if err != nil {
					return err
				}
				mt, err := testAccClient(t).GetMediaType(context.Background(), id)
				if err != nil {
					return err
				}
				if mt.Passwd != "" || mt.Username != "" {
					return fmt.Errorf("SMTP credentials must be cleared on a type change, got user=%q passwd-set=%v", mt.Username, mt.Passwd != "")
				}
				return nil
			}},
			{ResourceName: "zabbix_media_type.mail", ImportState: true, ImportStateVerify: true},
		},
	})
}

// --- zabbix_action ---

func TestAccAction_lifecycle(t *testing.T) {
	testAccPreCheck(t)
	name := acctest.RandomWithPrefix("tfacc-action")
	adminGroup := lookupID(t, "usergroup.get", "usrgrpid", map[string]interface{}{"name": "Zabbix administrators"})
	adminUser := lookupID(t, "user.get", "userid", map[string]interface{}{"username": "Admin"})

	base := testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_host_group" "a" { name = "%s-a" }
resource "zabbix_host_group" "b" {
  name = "%s-b"
  # Serialised on purpose: two concurrent hostgroup.create calls on a virgin
  # database race to insert the id-allocator row (ids_pkey duplicate key,
  # "Database error occurred") when this test runs first.
  depends_on = [zabbix_host_group.a]
}
resource "zabbix_media_type" "wh" {
  name   = "%s-wh"
  type   = 4
  script = "return 'OK';"
}
`, name, name, name)
	// Conditions are declared in an order different from what the API returns.
	cfg := func(n string, ops string) string {
		return base + fmt.Sprintf(`
resource "zabbix_action" "act" {
  name       = %q
  esc_period = "2h"
  evaltype   = 2
  condition {
    conditiontype = 3
    operator      = 2
    value         = "disk"
  }
  condition {
    conditiontype = 0
    value         = zabbix_host_group.b.id
  }
  condition {
    conditiontype = 0
    value         = zabbix_host_group.a.id
  }
  # Event tag value "env" contains "prod"
  condition {
    conditiontype = 26
    operator      = 2
    value         = "prod"
    value2        = "env"
  }
%s}`, n, ops)
	}
	opsBoth := fmt.Sprintf(`
  operation {
    mediatypeid = zabbix_media_type.wh.id
    default_msg = false
    subject     = "{TRIGGER.NAME}"
    message     = "{TRIGGER.STATUS}"
    user_groups = [%q]
    users       = [%q]
  }
  operation {
    esc_step_from = 2
    esc_step_to   = 3
    esc_period    = "30m"
    users         = [%q]
  }
`, adminGroup, adminUser, adminUser)
	opsUsersOnly := fmt.Sprintf(`
  pause_suppressed   = false
  pause_symptoms     = false
  notify_if_canceled = false
  operation {
    esc_step_to = 0
    esc_period  = "1800"
    users       = [%q]
  }
`, adminUser)
	// Clearing subject/message while keeping default_msg = false: the API
	// merges omitted opmessage fields, so this only converges when the empty
	// strings are transmitted explicitly.
	opsCleared := fmt.Sprintf(`
  operation {
    mediatypeid = zabbix_media_type.wh.id
    default_msg = false
    subject     = ""
    message     = ""
    users       = [%q]
  }
`, adminUser)
	opsMacro := fmt.Sprintf(`
  operation {
    esc_period = "{$TFACC.ESC}"
    users      = [%q]
  }
`, adminUser)

	var actionID string
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			testAccCheckGone(t, "zabbix_action.act", func(c *ZabbixClient, id string) error { _, err := c.GetAction(context.Background(), id); return err }),
			testAccCheckGone(t, "zabbix_media_type.wh", func(c *ZabbixClient, id string) error { _, err := c.GetMediaType(context.Background(), id); return err }),
			testAccCheckGone(t, "zabbix_host_group.a", func(c *ZabbixClient, id string) error { _, err := c.GetHostGroup(context.Background(), id); return err }),
			testAccCheckGone(t, "zabbix_host_group.b", func(c *ZabbixClient, id string) error { _, err := c.GetHostGroup(context.Background(), id); return err }),
		),
		Steps: []resource.TestStep{
			{Config: cfg(name, opsBoth), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_action.act", "condition.#", "4"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.#", "2"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.1.esc_step_from", "2"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.1.esc_period", "30m"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.user_groups.#", "1"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.users.#", "1"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.subject", "{TRIGGER.NAME}"),
				func(s *terraform.State) error {
					var err error
					actionID, err = stateID(s, "zabbix_action.act")
					return err
				},
			)},
			{
				// External drift of the filter must be repaired by an apply of
				// the unchanged configuration.
				PreConfig: func() {
					params := map[string]interface{}{
						"actionid": actionID,
						"filter":   map[string]interface{}{"evaltype": "0", "conditions": []interface{}{}},
					}
					if err := testAccClient(t).Call(context.Background(), "action.update", params, nil); err != nil {
						t.Fatal(err)
					}
				},
				Config: cfg(name, opsBoth),
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_action.act", "condition.#", "4"),
					func(s *terraform.State) error {
						action, err := testAccClient(t).GetAction(context.Background(), actionID)
						if err != nil {
							return err
						}
						if len(action.Filter.Conditions) != 4 {
							return fmt.Errorf("external condition drift not repaired: %d conditions", len(action.Filter.Conditions))
						}
						return nil
					},
				),
			},
			{Config: cfg(name, opsCleared), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.#", "1"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.default_msg", "false"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.subject", ""),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.message", ""),
			)},
			{Config: cfg(name+"-renamed", opsUsersOnly), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_action.act", "name", name+"-renamed"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.user_groups.#", "0"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.users.#", "1"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.esc_step_to", "0"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.mediatypeid", "0"),
				resource.TestCheckResourceAttr("zabbix_action.act", "pause_suppressed", "false"),
				resource.TestCheckResourceAttr("zabbix_action.act", "pause_symptoms", "false"),
				resource.TestCheckResourceAttr("zabbix_action.act", "notify_if_canceled", "false"),
			)},
			{
				// User macro as an operation step duration must round-trip.
				Config: cfg(name+"-renamed", opsMacro),
				Check:  resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.esc_period", "{$TFACC.ESC}"),
			},
			{ResourceName: "zabbix_action.act", ImportState: true, ImportStateVerify: true},
		},
	})
}

// TestAccProvider_TLSTerminatedProxy drives the real TLS client path
// (ca_cert_file -> RootCAs -> handshake) through a TLS-terminating reverse
// proxy in front of the real Zabbix API.
func TestAccProvider_TLSTerminatedProxy(t *testing.T) {
	testAccPreCheck(t)
	backend, err := url.Parse(os.Getenv("ZABBIX_URL"))
	if err != nil {
		t.Fatal(err)
	}
	// The reverse-proxy target must carry no path: SingleHostReverseProxy
	// joins the target path with the request path, which would silently turn
	// /api_jsonrpc.php into /api_jsonrpc.php/api_jsonrpc.php.
	target := *backend
	target.Path, target.RawPath = "", ""
	rp := httputil.NewSingleHostReverseProxy(&target)
	proxy := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != backend.Path {
			t.Errorf("proxy received path %q, want %q", r.URL.Path, backend.Path)
		}
		rp.ServeHTTP(w, r)
	}))
	proxy.StartTLS()
	t.Cleanup(proxy.Close)

	caPath := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: proxy.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewZabbixClient(ClientConfig{
		URL:        proxy.URL + backend.Path,
		Username:   os.Getenv("ZABBIX_USERNAME"),
		Password:   os.Getenv("ZABBIX_PASSWORD"),
		APIToken:   os.Getenv("ZABBIX_API_TOKEN"),
		CACertFile: caPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := c.GetVersion(ctx); err != nil {
		t.Fatalf("apiinfo.version over TLS: %v", err)
	}
	if err := c.Login(ctx); err != nil {
		t.Fatalf("login over TLS: %v", err)
	}

	// Without the CA file the self-signed proxy must be rejected.
	plain, err := NewZabbixClient(ClientConfig{URL: proxy.URL + backend.Path, APIToken: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plain.GetVersion(ctx); err == nil {
		t.Fatal("a self-signed certificate must be rejected without ca_cert_file")
	}

	// The same path through the real provider wiring (schema defaults included).
	raw := map[string]interface{}{"url": proxy.URL + backend.Path, "ca_cert_file": caPath}
	d := schema.TestResourceDataRaw(t, Provider().Schema, raw)
	if _, diags := providerConfigure(ctx, d); diags.HasError() {
		t.Fatalf("providerConfigure over the TLS proxy failed: %v", diags)
	}
}

// TestAccProvider_SessionRelogin invalidates the live session behind the
// client's back and proves the single-flight re-login against the real API.
func TestAccProvider_SessionRelogin(t *testing.T) {
	testAccPreCheck(t)
	if os.Getenv("ZABBIX_USERNAME") == "" {
		t.Skip("session re-login requires password credentials")
	}
	c, err := NewZabbixClient(ClientConfig{
		URL:      os.Getenv("ZABBIX_URL"),
		Username: os.Getenv("ZABBIX_USERNAME"),
		Password: os.Getenv("ZABBIX_PASSWORD"),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := c.Login(ctx); err != nil {
		t.Fatal(err)
	}

	// Count real user.login requests on the transport: the single-flight
	// property must hold against the live API, not only against mocks.
	var logins atomic.Int32
	base := c.httpClient.Transport
	c.httpClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		if strings.Contains(string(body), `"user.login"`) {
			logins.Add(1)
		}
		return base.RoundTrip(req)
	})

	// Invalidate the live session behind the client's back.
	if err := c.Call(ctx, "user.logout", []interface{}{}, nil); err != nil {
		t.Fatalf("user.logout: %v", err)
	}

	const parallel = 5
	var wg sync.WaitGroup
	errs := make(chan error, parallel)
	for i := 0; i < parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := c.GetHostGroup(ctx, "1")
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrNotFound) {
			t.Fatalf("re-login after an invalidated session failed: %v", err)
		}
	}
	if got := logins.Load(); got != 1 {
		t.Fatalf("parallel calls after session invalidation must share exactly one re-login, got %d", got)
	}
}

func TestAccProvider_APIToken(t *testing.T) {
	testAccPreCheck(t)
	if os.Getenv("ZABBIX_API_TOKEN") != "" {
		t.Skip("already authenticated with an API token; minting a test token requires password credentials")
	}
	c := testAccClient(t)
	ctx := context.Background()
	userID := testAccCurrentUserID(t, c)

	var created map[string][]string
	if err := c.Call(ctx, "token.create", map[string]interface{}{"name": acctest.RandomWithPrefix("tfacc"), "userid": userID}, &created); err != nil {
		t.Fatalf("token.create: %v", err)
	}
	tokenID := created["tokenids"][0]
	t.Cleanup(func() {
		// An ignored failure would leave an ACTIVE token on the target
		// Zabbix; a fresh deadline keeps the cleanup from hanging forever.
		cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := c.Call(cctx, "token.delete", []string{tokenID}, nil); err != nil {
			t.Errorf("cleanup: token.delete %s failed, the token may still be active: %v", tokenID, err)
		}
	})

	var generated []map[string]string
	if err := c.Call(ctx, "token.generate", []string{tokenID}, &generated); err != nil {
		t.Fatalf("token.generate: %v", err)
	}

	name := acctest.RandomWithPrefix("tfacc-token")
	cfg := func(group, mtName string) string {
		return testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_host_group" "g" { name = %q }

resource "zabbix_media_type" "wh" {
  name   = %q
  type   = 4
  script = "return 'OK';"
}`, group, mtName)
	}

	// Authenticate with the token only; username/password must not leak in.
	// A full lifecycle (create, update, import, destroy) runs on the Bearer
	// path, not just a single create.
	t.Setenv("ZABBIX_API_TOKEN", generated[0]["token"])
	t.Setenv("ZABBIX_USERNAME", "")
	t.Setenv("ZABBIX_PASSWORD", "")
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy: resource.ComposeTestCheckFunc(
			testAccCheckGone(t, "zabbix_media_type.wh", func(c *ZabbixClient, id string) error { _, err := c.GetMediaType(context.Background(), id); return err }),
			testAccCheckGone(t, "zabbix_host_group.g", func(c *ZabbixClient, id string) error { _, err := c.GetHostGroup(context.Background(), id); return err }),
		),
		Steps: []resource.TestStep{
			{Config: cfg(name+"-grp", name+"-wh"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_host_group.g", "name", name+"-grp"),
				resource.TestCheckResourceAttr("zabbix_media_type.wh", "name", name+"-wh"),
			)},
			{Config: cfg(name+"-grp2", name+"-wh2"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_host_group.g", "name", name+"-grp2"),
				resource.TestCheckResourceAttr("zabbix_media_type.wh", "name", name+"-wh2"),
			)},
			{ResourceName: "zabbix_media_type.wh", ImportState: true, ImportStateVerify: true},
			{ResourceName: "zabbix_host_group.g", ImportState: true, ImportStateVerify: true},
		},
	})
}

// testAccCurrentUserID resolves the user behind the configured credentials
// (works for both password sessions and API tokens).
func testAccCurrentUserID(t *testing.T, c *ZabbixClient) string {
	t.Helper()
	ctx := context.Background()
	params := map[string]string{}
	if c.apiToken != "" {
		params["token"] = c.apiToken
	} else {
		if err := c.Login(ctx); err != nil {
			t.Fatal(err)
		}
		params["sessionid"] = c.currentToken()
	}
	var user map[string]interface{}
	// user.checkAuthentication must be called without an Authorization header.
	if err := c.rawCall(ctx, "user.checkAuthentication", params, "", &user); err != nil {
		t.Fatalf("user.checkAuthentication: %v", err)
	}
	return user["userid"].(string)
}

func TestAccMediaType_scriptSmsTypeChange(t *testing.T) {
	name := acctest.RandomWithPrefix("tfacc-mt")
	cfg := func(body string) string {
		return testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_media_type" "mt" {
  name = %q
%s}`, name, body)
	}
	script := `
  type      = 1
  exec_path = "notify.sh"
`
	sms := `
  type      = 2
  gsm_modem = "/dev/ttyS0"
`
	webhook := `
  type   = 4
  script = "return 'OK';"
`
	webhookWithParam := webhook + `
  parameter {
    name  = "url"
    value = "https://example.test"
  }
`
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_media_type.mt", func(c *ZabbixClient, id string) error { _, err := c.GetMediaType(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg(script), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "type", "1"),
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "exec_path", "notify.sh"),
			)},
			{ResourceName: "zabbix_media_type.mt", ImportState: true, ImportStateVerify: true},
			// Type changes must not leave stale attributes of the previous type in state.
			{Config: cfg(sms), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "type", "2"),
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "gsm_modem", "/dev/ttyS0"),
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "exec_path", ""),
			)},
			{ResourceName: "zabbix_media_type.mt", ImportState: true, ImportStateVerify: true},
			{Config: cfg(webhook), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "type", "4"),
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "gsm_modem", ""),
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "parameter.#", "0"),
			)},
			{Config: cfg(webhookWithParam), Check: resource.TestCheckResourceAttr("zabbix_media_type.mt", "parameter.#", "1")},
			// Webhook parameters must be cleared on the way back to a script type,
			// otherwise the script would be refused as unmanageable.
			{Config: cfg(script), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "type", "1"),
				resource.TestCheckResourceAttr("zabbix_media_type.mt", "parameter.#", "0"),
			)},
			{ResourceName: "zabbix_media_type.mt", ImportState: true, ImportStateVerify: true},
		},
	})
}
