package zabbix

import (
	"context"
	"fmt"
	"os"
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
		for k, rs := range s.RootModule().Resources {
			if k != addr {
				continue
			}
			if err := get(testAccClient(t), rs.Primary.ID); err != ErrNotFound {
				return fmt.Errorf("%s %s still exists (err=%v)", addr, rs.Primary.ID, err)
			}
		}
		return nil
	}
}

// --- zabbix_host ---

func TestAccHost_lifecycle(t *testing.T) {
	testAccPreCheck(t)
	name := acctest.RandomWithPrefix("tfacc-host")
	templateA := lookupID(t, "template.get", "templateid", map[string]interface{}{"host": "Linux by Zabbix agent"})
	templateB := lookupID(t, "template.get", "templateid", map[string]interface{}{"host": "Zabbix server health"})

	base := testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_host_group" "g" { name = "%s-grp" }
`, name)
	cfgIP := base + fmt.Sprintf(`
resource "zabbix_host" "h" {
  host      = %q
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
  host   = %q
  groups = [zabbix_host_group.g.id]
  use_ip = false
  dns    = "agent.example.test"
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
				Config: cfgDNS,
				Check: resource.ComposeTestCheckFunc(
					resource.TestCheckResourceAttr("zabbix_host.h", "use_ip", "false"),
					resource.TestCheckResourceAttr("zabbix_host.h", "dns", "agent.example.test"),
					resource.TestCheckResourceAttr("zabbix_host.h", "ip", ""),
					resource.TestCheckResourceAttr("zabbix_host.h", "templates.#", "0"),
					testAccCheckHostInterfaces(t, "zabbix_host.h", 2),
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
		return nil
	}
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
			{Config: cfg(name, twoParams), Check: resource.TestCheckResourceAttr("zabbix_media_type.wh", "parameter.#", "2")},
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
	cfg := testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_media_type" "mail" {
  name                = %q
  type                = 0
  enabled             = false
  smtp_server         = "mail.example.test"
  smtp_port           = 587
  smtp_helo           = "example.test"
  smtp_email          = "zabbix@example.test"
  smtp_security       = 1
  smtp_verify_peer    = true
  smtp_authentication = 1
  username            = "zabbix"
  password            = "hunter2"
}`, name)
	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_media_type.mail", func(c *ZabbixClient, id string) error { _, err := c.GetMediaType(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg, Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "enabled", "false"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "smtp_port", "587"),
				resource.TestCheckResourceAttr("zabbix_media_type.mail", "password", "hunter2"),
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
`, adminGroup, adminUser)
	opsUsersOnly := fmt.Sprintf(`
  operation {
    esc_step_to = 0
    users       = [%q]
  }
`, adminUser)

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_action.act", func(c *ZabbixClient, id string) error { _, err := c.GetAction(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg(name, opsBoth), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_action.act", "condition.#", "4"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.user_groups.#", "1"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.users.#", "1"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.subject", "{TRIGGER.NAME}"),
			)},
			{Config: cfg(name+"-renamed", opsUsersOnly), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_action.act", "name", name+"-renamed"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.user_groups.#", "0"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.users.#", "1"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.esc_step_to", "0"),
				resource.TestCheckResourceAttr("zabbix_action.act", "operation.0.mediatypeid", "0"),
			)},
			{ResourceName: "zabbix_action.act", ImportState: true, ImportStateVerify: true},
		},
	})
}

// --- provider: API token authentication ---

func TestAccProvider_APIToken(t *testing.T) {
	testAccPreCheck(t)
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
		},
	})
}
