# Sourced by scripts/service-test.sh. It says what "the service works" means for this
# program; everything else in the service test is the same for every SpliceFS program.

# The default listener of the server (docs/configuration.md): every interface, TLS with a
# certificate the server makes for itself. The API lives under the default base path /api
# and the health endpoint is /v1/health (api/routes.go).
port=8480
health_path=/api/v1/health
answers_text="answers 200 on $health_path from outside the container"

expected_user="$(sed -n 's/^User=//p' "packaging/systemd/$program.service")"
[ -n "$expected_user" ] || fail "packaging/systemd/$program.service sets no User"

# Extra options for docker run: the port is published on a free port of the host.
run_options() {
    printf '%s' "-p 127.0.0.1::$port"
}

# Status 0 when the health endpoint answers 200, asked from the host. Docker accepts the
# connection on the host even when nothing listens inside, so only the answer counts. The
# published port can change when the container restarts, so it is looked up every time.
answers() {
    hostport="$(docker port "$1" "$port/tcp" | head -n 1 | sed 's/.*://')" || return 1
    code="$(curl -sk -o /dev/null -w '%{http_code}' --max-time 3 \
        "https://127.0.0.1:$hostport$health_path" || true)"
    [ "$code" = 200 ]
}
