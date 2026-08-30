resource "zabbix_host_group" "servers" {
  name = "Linux servers"
}

# Host monitored through its IP address.
resource "zabbix_host" "web01" {
  host        = "web01"
  name        = "Web server 01"
  description = "Managed by Terraform"
  groups      = [zabbix_host_group.servers.id]
  templates   = ["10001"] # Linux by Zabbix agent
  ip          = "192.0.2.10"
  port        = "10050"
}

# Host monitored through its DNS name.
resource "zabbix_host" "db01" {
  host   = "db01"
  groups = [zabbix_host_group.servers.id]
  use_ip = false
  dns    = "db01.example.com"
}
