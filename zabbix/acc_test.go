package zabbix

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"testing"

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
}

// testAccClient returns a raw API client for test setup and verification.
func testAccClient(t *testing.T) *ZabbixClient {
	t.Helper()
	c, err := NewZabbixClient(ClientConfig{
		URL:      os.Getenv("ZABBIX_URL"),
		Username: os.Getenv("ZABBIX_USERNAME"),
		Password: os.Getenv("ZABBIX_PASSWORD"),
		APIToken: os.Getenv("ZABBIX_API_TOKEN"),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
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
`, name)
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
  groups  = [zabbix_host_group.g.id]
  use_ip  = false
  dns     = "agent.example.test"
}`, name)

	var hostID string
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_host.h", func(c *ZabbixClient, id string) error { _, err := c.GetHost(context.Background(), id); return err }),
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
		CheckDestroy:      testAccCheckGone(t, "zabbix_host.h", func(c *ZabbixClient, id string) error { _, err := c.GetHost(context.Background(), id); return err }),
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
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "max_attempts", "5"),
			)},
			// Update without a type change must round-trip the changed values.
			{Config: cfg(2525, "hunter3"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "smtp_port", "2525"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "password", "hunter3"),
			)},
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
resource "zabbix_host_group" "b" { name = "%s-b" }
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
		CheckDestroy:      testAccCheckGone(t, "zabbix_action.act", func(c *ZabbixClient, id string) error { _, err := c.GetAction(context.Background(), id); return err }),
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
	proxy := httptest.NewUnstartedServer(httputil.NewSingleHostReverseProxy(backend))
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
}

// --- provider: API token authentication ---

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
	t.Cleanup(func() { _ = c.Call(ctx, "token.delete", []string{tokenID}, nil) })

	var generated []map[string]string
	if err := c.Call(ctx, "token.generate", []string{tokenID}, &generated); err != nil {
		t.Fatalf("token.generate: %v", err)
	}

	name := acctest.RandomWithPrefix("tfacc-token-group")
	cfg := testAccProviderConfig() + fmt.Sprintf(`resource "zabbix_host_group" "g" { name = %q }`, name)

	// Authenticate with the token only; username/password must not leak in.
	t.Setenv("ZABBIX_API_TOKEN", generated[0]["token"])
	t.Setenv("ZABBIX_USERNAME", "")
	t.Setenv("ZABBIX_PASSWORD", "")
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_host_group.g", func(c *ZabbixClient, id string) error { _, err := c.GetHostGroup(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg, Check: resource.TestCheckResourceAttr("zabbix_host_group.g", "name", name)},
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
