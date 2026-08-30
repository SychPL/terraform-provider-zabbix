# Webhook media type; secrets belong in parameters (marked sensitive), not in the script.
resource "zabbix_media_type" "slack" {
  name    = "Slack"
  type    = 4
  timeout = "10s"
  script  = <<-EOT
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
}
