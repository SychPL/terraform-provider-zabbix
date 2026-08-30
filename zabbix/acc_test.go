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

func testAccClient(t *testing.T) *ZabbixClient {
	t.Helper()
	c, err := NewZabbixClient(
		os.Getenv("ZABBIX_URL"),
		os.Getenv("ZABBIX_USERNAME"),
		os.Getenv("ZABBIX_PASSWORD"),
		os.Getenv("ZABBIX_API_TOKEN"),
		true, // tlsInsecure for local self-signed dev test docker
		"",
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func testAccProviderConfig() string {
	return `provider "zabbix" {}` + "\n"
}

func testAccCheckGone(t *testing.T, addr string, get func(*ZabbixClient, string) error) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		for k, rs := range s.RootModule().Resources {
			if k != addr {
				continue
			}
			if err := get(testAccClient(t), rs.Primary.ID); err != ErrNotFound {
				if err != nil {
					return fmt.Errorf("unexpected error checking %s gone: %w", addr, err)
				}
				return fmt.Errorf("resource %s still exists", addr)
			}
		}
		return nil
	}
}

// --- HOST GROUP ACCEPTANCE TESTS ---

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

// --- HOST ACCEPTANCE TESTS ---

func TestAccHost_basic(t *testing.T) {
	name := acctest.RandomWithPrefix("tfacc-host")
	cfg := func(n, ip string) string {
		return testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_host_group" "g" {
  name = "%s-group"
}

resource "zabbix_host" "h" {
  host   = %q
  groups = [zabbix_host_group.g.id]
  ip     = %q
  port   = "10050"
}
`, name, n, ip)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_host.h", func(c *ZabbixClient, id string) error { _, err := c.GetHost(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg(name, "192.0.2.10"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_host.h", "host", name),
				resource.TestCheckResourceAttr("zabbix_host.h", "ip", "192.0.2.10"),
				resource.TestCheckResourceAttr("zabbix_host.h", "use_ip", "true"),
			)},
			{Config: cfg(name+"-renamed", "192.0.2.11"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_host.h", "host", name+"-renamed"),
				resource.TestCheckResourceAttr("zabbix_host.h", "ip", "192.0.2.11"),
			)},
			{ResourceName: "zabbix_host.h", ImportState: true, ImportStateVerify: true},
		},
	})
}

// --- MEDIA TYPE ACCEPTANCE TESTS ---

func TestAccMediaType_basic(t *testing.T) {
	name := acctest.RandomWithPrefix("tfacc-media")
	cfg := func(n, path string) string {
		return testAccProviderConfig() + fmt.Sprintf(`
resource "zabbix_media_type" "m" {
  name      = %q
  type      = 1
  exec_path = %q
}
`, n, path)
	}

	resource.Test(t, resource.TestCase{
		PreCheck:          func() { testAccPreCheck(t) },
		ProviderFactories: testAccProviderFactories,
		CheckDestroy:      testAccCheckGone(t, "zabbix_media_type.m", func(c *ZabbixClient, id string) error { _, err := c.GetMediaType(context.Background(), id); return err }),
		Steps: []resource.TestStep{
			{Config: cfg(name, "alert.sh"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.m", "name", name),
				resource.TestCheckResourceAttr("zabbix_media_type.m", "exec_path", "alert.sh"),
			)},
			{Config: cfg(name+"-renamed", "alert-v2.sh"), Check: resource.ComposeTestCheckFunc(
				resource.TestCheckResourceAttr("zabbix_media_type.m", "name", name+"-renamed"),
				resource.TestCheckResourceAttr("zabbix_media_type.m", "exec_path", "alert-v2.sh"),
			)},
			{ResourceName: "zabbix_media_type.m", ImportState: true, ImportStateVerify: true},
		},
	})
}
