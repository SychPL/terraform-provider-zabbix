variable "slack_webhook_url" {
  type      = string
  sensitive = true
}

variable "smtp_password" {
  type      = string
  sensitive = true
}

# Webhook media type; secrets belong in parameters (marked sensitive), not in the script.
resource "zabbix_media_type" "slack" {
  name    = "Slack"
  type    = 4
  timeout = "10s"

  max_attempts     = 5
  attempt_interval = "30s"

  show_event_menu = true
  event_menu_url  = "https://slack.example.com/archives/{EVENT.TAGS.channel}"
  event_menu_name = "Open Slack channel"
  script          = <<-EOT
    var params = JSON.parse(value);
    var req = new HttpRequest();
    req.addHeader('Content-Type: application/json');
    req.post(params.url, JSON.stringify({ text: params.message }));
    if (req.getStatus() !== 200) {
      throw 'Slack responded with HTTP ' + req.getStatus();
    }
    return 'OK';
  EOT

  parameter {
    name  = "url"
    value = var.slack_webhook_url
  }
  parameter {
    name  = "message"
    value = "{TRIGGER.NAME}: {TRIGGER.STATUS}"
  }
}

# Email media type with SMTP authentication and STARTTLS.
resource "zabbix_media_type" "email" {
  name                = "Email"
  type                = 0
  smtp_server         = "smtp.example.com"
  smtp_port           = 587
  smtp_helo           = "example.com"
  smtp_email          = "zabbix@example.com"
  smtp_security       = 1
  smtp_verify_peer    = true
  smtp_verify_host    = true
  smtp_authentication = 1
  username            = "zabbix"
  password            = var.smtp_password
  content_type        = 0
}
