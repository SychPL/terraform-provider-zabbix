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

resource "zabbix_media_type" "slack_webhook" {
  name    = "Slack Notifications Webhook"
  type    = 4 # Webhook
  enabled = true
  script  = "var params = JSON.parse(value);\nvar req = new HttpRequest();\nreq.post(params.url, JSON.stringify({text: params.message}));\nreturn 'OK';"
  timeout = "30s"

  parameter {
    name  = "url"
    value = "http://localhost/slack-webhook"
  }
  parameter {
    name  = "message"
    value = "{TRIGGER.NAME}: {TRIGGER.STATUS}"
  }
}

resource "zabbix_action" "alert_action" {
  name        = "Send Slack Alert for Critical Hosts"
  eventsource = 0 # Triggers
  enabled     = true
  esc_period  = "1h"
  evaltype    = 0 # AND/OR

  condition {
    conditiontype = 0  # Host group (0 in Zabbix trigger actions)
    operator      = 0  # equals (0 in Zabbix host group condition)
    value         = zabbix_host_group.servers.id
  }

  operation {
    operationtype = 0 # Send message
    esc_period    = "0"
    esc_step_from = 1
    esc_step_to   = 1
    mediatypeid   = zabbix_media_type.slack_webhook.id
    default_msg   = false
    subject       = "Zabbix Alert: {TRIGGER.NAME}"
    message       = "Problem detected on server: {TRIGGER.NAME} - status {TRIGGER.STATUS}"
    user_groups   = ["7"]
  }
}
