{
  email {$ACME_EMAIL}
}

{$CONSOLE_DOMAIN} {
  reverse_proxy relayops:9090 {
    flush_interval -1
  }
}

# API traffic is spread over both gateways. A gateway that is not ready
# (no config loaded yet, or stopped for a rolling update) is taken out of
# rotation by the active check; a failed request is retried on the other
# gateway when it is safe to do so (connection errors before any response).
{$API_DOMAIN} {
  reverse_proxy gateway-a:8080 gateway-b:8080 {
    lb_policy round_robin
    lb_try_duration 5s
    health_uri /readyz
    health_port 9091
    health_interval 2s
    health_timeout 1s
    fail_duration 10s
    flush_interval -1
  }
}
