terraform {
  required_providers {
    zabbix = {
      source = "adi/zabbix"
    }
  }
}

provider "zabbix" {
  url      = "http://localhost:8082/api_jsonrpc.php"
  username = "Admin"
  password = "zabbix"
}

resource "zabbix_host_group" "servers" {
  name = "Local Test Servers Group"
}

resource "zabbix_host" "test_host" {
  host   = "local-test-agent-host-updated"
  groups = [zabbix_host_group.servers.id]
  ip     = "192.168.1.5"
  port   = "10050"
}
