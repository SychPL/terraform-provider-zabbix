variable "linux_template_id" {
  type        = string
  description = "ID of the agent template to link, e.g. template.get filter host=\"Linux by Zabbix agent\"."
}

resource "zabbix_host_group" "servers" {
  name = "Linux servers"
}

# Host monitored through its IP address.
resource "zabbix_host" "web01" {
  host        = "web01"
  name        = "Web server 01"
  description = "Managed by Terraform"
  groups      = [zabbix_host_group.servers.id]
  templates   = [var.linux_template_id] # e.g. "Linux by Zabbix agent" on your instance
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
