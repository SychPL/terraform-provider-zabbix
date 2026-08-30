resource "zabbix_host_group" "servers" {
  name = "Linux servers"
}

resource "zabbix_media_type" "slack" {
  name   = "Slack"
  type   = 4
  script = "return 'OK';"
}

resource "zabbix_action" "notify_slack" {
  name       = "Notify Slack about server problems"
  esc_period = "1h"
  evaltype   = 0 # And/Or

  # Host group equals "Linux servers"
  condition {
    conditiontype = 0
    operator      = 0
    value         = zabbix_host_group.servers.id
  }

  # Trigger severity >= High (4)
  condition {
    conditiontype = 4
    operator      = 5
    value         = "4"
  }

  operation {
    mediatypeid = zabbix_media_type.slack.id
    default_msg = false
    subject     = "Problem: {EVENT.NAME}"
    message     = "Host: {HOST.NAME}\nSeverity: {EVENT.SEVERITY}"
    user_groups = ["7"] # Zabbix administrators
  }
}
