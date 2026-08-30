terraform {
  required_providers {
    zabbix = {
      source = "Tensai123/zabbix"
    }
  }
}

# Authenticate with an API token (recommended) ...
provider "zabbix" {
  url       = "https://zabbix.example.com/api_jsonrpc.php"
  api_token = var.zabbix_api_token # or ZABBIX_API_TOKEN
}

# ... or with username and password (also possible through the
# ZABBIX_USERNAME and ZABBIX_PASSWORD environment variables):
# provider "zabbix" {
#   url      = "https://zabbix.example.com/api_jsonrpc.php"
#   username = "Admin"
#   password = var.zabbix_password # declare it as a sensitive variable
# }
