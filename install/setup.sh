#!/usr/bin/env bash
#
# testMO agent installer. Run once on a server whose RPC node is already up;
# safe to re-run (nothing is restarted unless its config actually changed).
#
#   sudo ./install/setup.sh --chain geth --node-id abc123
#
set -euo pipefail

# Pinned deliberately: "latest" would make installs unreproducible and depend
# on the GitHub API at run time. Bump these after checking the release pages.
NODE_EXPORTER_VERSION="1.8.2"
PROMETHEUS_VERSION="2.53.0"

RETENTION="30d"
AGENT_PORT="3001"
API_PORT="9443"

# Where Let's Encrypt's HTTP-01 challenge files are written and served from.
# Used only when --domain is passed.
ACME_WEBROOT="/var/www/testmo-acme"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

CHAIN="geth"
METRICS_PORT=""
METRICS_PATH=""
NODE_ID=""
DOMAIN=""
LE_EMAIL=""
ALLOW_ORIGINS=()
RESTART_AGENT=0
RELOAD_NGINX=0

log()  { printf '  %s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*" >&2; }
die()  { printf '\nERROR: %s\n' "$*" >&2; exit 1; }

usage() {
	cat <<EOF
Usage: sudo $0 [options]

  --chain NAME          chain config from chains/ (default: geth)
  --metrics-port PORT   node's Prometheus port   (default: from chain config)
  --metrics-path PATH   node's Prometheus path   (default: from chain config)
  --node-id ID          identifier reported by /api/v1/info (default: none)
  --allow-origin URL    browser origin allowed to read the API; repeat for
                        more. Omit entirely to allow any origin (default).
                          --allow-origin https://dashboard.argusnodes.com \\
                          --allow-origin http://localhost:4322
  --domain FQDN         serve the API over HTTPS on this hostname, with a
                        Let's Encrypt certificate on :$API_PORT. Omit for plain
                        HTTP, which no browser on an https:// page can call.
  --email ADDR          contact address for certificate expiry warnings

--domain names ONE SERVER, not one customer: there is one agent per box and it
serves one chain config, so a customer with two nodes has two boxes and two
hostnames. Use an opaque name keyed to the Supabase order id —

  n-7f3k2q.nodes.argusnodes.com

— never the customer's name. Every certificate is published to public
Certificate Transparency logs, so customer-named subdomains would put our
entire customer list on crt.sh for anyone to enumerate. The dashboard reads
this hostname out of the Supabase row and the customer never types it, so the
opaque form costs nothing.

Before running with --domain: the A record must already point at this server's
public IP, and port 80 must be reachable from the internet — at install and at
every renewal thereafter.
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--chain)        CHAIN="${2:-}";        shift 2 ;;
		--metrics-port) METRICS_PORT="${2:-}"; shift 2 ;;
		--metrics-path) METRICS_PATH="${2:-}"; shift 2 ;;
		--node-id)      NODE_ID="${2:-}";      shift 2 ;;
		--allow-origin)
			# Checked here rather than left to fail silently: an origin the
			# browser never sends simply doesn't match, and the only symptom
			# is a CORS error in a console you can't see from this server.
			[[ "${2:-}" == *"://"* ]] ||
				die "--allow-origin wants a full origin like https://dashboard.argusnodes.com, got: ${2:-}"
			ALLOW_ORIGINS+=("${2%/}")
			shift 2 ;;
		--domain)       DOMAIN="${2:-}";       shift 2 ;;
		--email)        LE_EMAIL="${2:-}";     shift 2 ;;
		-h|--help)      usage; exit 0 ;;
		*)              usage; die "unknown flag: $1" ;;
	esac
done

# Reads a top-level scalar out of a YAML file. Commented-out keys are ignored,
# which is how an absent metrics_port disables the rpc_client scrape job.
yaml_scalar() {
	sed -n "s/^$1:[[:space:]]*//p" "$2" | head -n1 |
		sed -e 's/[[:space:]]*#.*$//' -e 's/[[:space:]]*$//' | tr -d "\"'"
}

# Writes stdin to a path, leaving it untouched when the content is identical.
# Returns 1 when unchanged, so callers can skip needless service restarts.
write_file() {
	local path="$1" mode="$2" tmp
	tmp="$(mktemp)"
	cat >"$tmp"
	chmod "$mode" "$tmp"
	if [[ -f "$path" ]] && cmp -s "$tmp" "$path"; then
		rm -f "$tmp"
		return 1
	fi
	mv "$tmp" "$path"
	return 0
}

ensure_user() {
	id -u "$1" &>/dev/null || useradd --system --no-create-home --shell /usr/sbin/nologin "$1"
}

# Fills the {{...}} placeholders in one of the install/*.tmpl nginx files.
render() {
	sed -e "s|{{DOMAIN}}|$DOMAIN|g" \
	    -e "s|{{ACME_WEBROOT}}|$ACME_WEBROOT|g" \
	    "$SCRIPT_DIR/$1"
}

# Tests the config and reloads, or backs the whole of testMO out of nginx.
#
# The same nginx is serving the customer's RPC on :443. A config that doesn't
# parse would take that down on the next reload for reasons that have nothing
# to do with them, so on rejection every file this script installs is removed
# and the run aborts. Any new testMO nginx file MUST be added to the rm below.
nginx_reload_or_revert() {
	if nginx -t >/dev/null 2>&1; then
		systemctl reload nginx
		return 0
	fi
	rm -f /etc/nginx/sites-enabled/testmo \
	      /etc/nginx/sites-enabled/testmo-acme \
	      /etc/nginx/conf.d/testmo-ratelimit.conf
	nginx -t || true
	die "nginx rejected the config — reverted, your existing sites are untouched"
}

# Creates a sites-enabled symlink, reporting whether it had to change, so a
# re-run that alters nothing doesn't reload nginx for nothing.
link_site() {
	local name="$1" link="/etc/nginx/sites-enabled/$1"
	if [[ -L "$link" && "$(readlink "$link")" == "../sites-available/$name" ]]; then
		return 1
	fi
	ln -sfn "../sites-available/$name" "$link"
	return 0
}

# This server's public IP, as the outside world (and Let's Encrypt) sees it.
# Several providers are tried because any one of them can be down, and a
# transient outage must not read as "your DNS is wrong".
public_ip() {
	local url ip
	for url in https://api.ipify.org https://checkip.amazonaws.com https://icanhazip.com; do
		ip="$(curl -fsS --max-time 5 "$url" 2>/dev/null | tr -d '[:space:]')" || true
		if [[ "$ip" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]]; then
			printf '%s' "$ip"
			return 0
		fi
	done
	return 1
}

# Every A record for a name, one per line. dig is the right tool but lives in
# dnsutils, which isn't installed by default; getent is always present, at the
# cost of also consulting /etc/hosts.
resolve_a() {
	if command -v dig >/dev/null; then
		dig +short A "$1" 2>/dev/null | grep -E '^[0-9]{1,3}(\.[0-9]{1,3}){3}$' || true
	else
		getent ahostsv4 "$1" 2>/dev/null | awk '{print $1}' | sort -u || true
	fi
}

# --------------------------------------------------------------------------
step "Checking prerequisites"

[[ $EUID -eq 0 ]] || die "must run as root (try: sudo $0 $*)"
[[ "$(uname -m)" == "x86_64" ]] || die "only linux/amd64 is supported (found $(uname -m))"

# shellcheck source=/dev/null
[[ -r /etc/os-release ]] && . /etc/os-release
[[ "${ID:-}" == "ubuntu" ]] || die "expected Ubuntu, found ${ID:-unknown}"
case "${VERSION_ID:-}" in
	22.04|24.04) ;;
	*) warn "untested on Ubuntu ${VERSION_ID:-unknown}; continuing" ;;
esac

for cmd in curl tar openssl systemctl nginx cmp; do
	command -v "$cmd" >/dev/null || die "$cmd is required but not installed"
done

if [[ -n "$DOMAIN" ]]; then
	# A bare hostname is the likely mistake, and Let's Encrypt cannot issue for
	# one. Rejecting here beats a certbot error four steps further in.
	[[ "$DOMAIN" != *://* && "$DOMAIN" != */* && "$DOMAIN" != *:* ]] ||
		die "--domain wants a bare FQDN, not a URL: $DOMAIN"
	[[ "$DOMAIN" == *.* ]] ||
		die "--domain needs a fully-qualified name like n-7f3k2q.nodes.argusnodes.com, not the bare host: $DOMAIN"
	[[ "$DOMAIN" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)+$ ]] ||
		die "--domain is not a valid hostname: $DOMAIN"
	[[ ! "$DOMAIN" =~ ^[0-9]{1,3}(\.[0-9]{1,3}){3}$ ]] ||
		die "--domain must be a hostname; Let's Encrypt will not issue for an IP address: $DOMAIN"
	[[ ${#DOMAIN} -le 253 ]] || die "--domain is longer than 253 characters"

	# The name is per SERVER, not per customer — one agent, one chain config,
	# one box. And it must be opaque: certificates land in public Certificate
	# Transparency logs, so a customer-named subdomain publishes that customer's
	# relationship with us to anyone reading crt.sh.
	case "$DOMAIN" in
		*.nodes.argusnodes.com) ;;
		*) warn "$DOMAIN is outside *.nodes.argusnodes.com — if it encodes a customer"
		   warn "name it will be published to Certificate Transparency logs and be"
		   warn "enumerable on crt.sh. Prefer an opaque, per-server name." ;;
	esac

	# Checked now, before anything is installed. An unpointed A record is the
	# most common way this fails, and left to certbot it surfaces as an opaque
	# challenge error that names neither address involved. It also burns one of
	# Let's Encrypt's five-failures-per-hostname-per-hour, so a couple of blind
	# retries lock the hostname out for an hour.
	PUBLIC_IP="$(public_ip)" ||
		die "could not determine this server's public IP (tried ipify, AWS, icanhazip) — check outbound HTTPS, then re-run"

	RESOLVED="$(resolve_a "$DOMAIN")"
	[[ -n "$RESOLVED" ]] ||
		die "$DOMAIN has no A record. Point it at $PUBLIC_IP, wait for the TTL, then re-run."

	if ! grep -qx "$PUBLIC_IP" <<<"$RESOLVED"; then
		warn "$DOMAIN resolves to: $(tr '\n' ' ' <<<"$RESOLVED")"
		warn "this server's public IP is: $PUBLIC_IP"
		die "$DOMAIN does not point at this server. Set its A record to $PUBLIC_IP, wait for the TTL to expire, then re-run. Let's Encrypt validates over HTTP-01 and would fail the same way, less legibly."
	fi
	log "domain: $DOMAIN -> $PUBLIC_IP (this server)"

	# Installed only for --domain, so a plain-HTTP install stays as light as it
	# has always been.
	if command -v certbot >/dev/null; then
		log "certbot: $(certbot --version 2>&1 | head -n1)"
	else
		log "installing certbot"
		DEBIAN_FRONTEND=noninteractive apt-get update -qq ||
			die "apt-get update failed — install certbot manually, then re-run"
		DEBIAN_FRONTEND=noninteractive apt-get install -y -qq certbot >/dev/null ||
			die "could not install certbot"
	fi
else
	warn "no --domain: the API will be plain HTTP, which every browser blocks as"
	warn "mixed content from an https:// page. The dashboard cannot use this"
	warn "server until you re-run with --domain <fqdn>."
fi

CHAIN_SRC="$REPO_DIR/chains/$CHAIN.yaml"
AGENT_SRC="$REPO_DIR/testmo-agent"
[[ -f "$CHAIN_SRC" ]] || die "no chain config at $CHAIN_SRC"
[[ -f "$AGENT_SRC" ]] || die "no agent binary at $AGENT_SRC (run 'make build' first)"

# Flags win; otherwise take the node's metrics endpoint from the chain config.
[[ -n "$METRICS_PORT" ]] || METRICS_PORT="$(yaml_scalar metrics_port "$CHAIN_SRC")"
[[ -n "$METRICS_PATH" ]] || METRICS_PATH="$(yaml_scalar metrics_path "$CHAIN_SRC")"
log "chain: $CHAIN"
if [[ -n "$METRICS_PORT" ]]; then
	log "node metrics: 127.0.0.1:${METRICS_PORT}${METRICS_PATH}"
else
	log "node metrics: none declared — RPC probes only"
fi

# --------------------------------------------------------------------------
step "Installing node_exporter $NODE_EXPORTER_VERSION"

if [[ -x /usr/local/bin/node_exporter ]] &&
	/usr/local/bin/node_exporter --version 2>&1 | grep -qF "version $NODE_EXPORTER_VERSION"; then
	log "already installed"
else
	tmp="$(mktemp -d)"
	curl -fsSL --retry 3 \
		"https://github.com/prometheus/node_exporter/releases/download/v${NODE_EXPORTER_VERSION}/node_exporter-${NODE_EXPORTER_VERSION}.linux-amd64.tar.gz" |
		tar xz -C "$tmp" --strip-components=1
	install -m 0755 "$tmp/node_exporter" /usr/local/bin/node_exporter
	rm -rf "$tmp"
	log "installed"
fi

ensure_user node_exporter
write_file /etc/systemd/system/node_exporter.service 0644 <<EOF || true
[Unit]
Description=Prometheus Node Exporter
After=network-online.target
Wants=network-online.target

[Service]
User=node_exporter
Group=node_exporter
ExecStart=/usr/local/bin/node_exporter --web.listen-address=127.0.0.1:9100
Restart=always
RestartSec=5s
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

# --------------------------------------------------------------------------
step "Installing Prometheus $PROMETHEUS_VERSION"

if [[ -x /usr/local/bin/prometheus ]] &&
	/usr/local/bin/prometheus --version 2>&1 | grep -qF "version $PROMETHEUS_VERSION"; then
	log "already installed"
else
	tmp="$(mktemp -d)"
	curl -fsSL --retry 3 \
		"https://github.com/prometheus/prometheus/releases/download/v${PROMETHEUS_VERSION}/prometheus-${PROMETHEUS_VERSION}.linux-amd64.tar.gz" |
		tar xz -C "$tmp" --strip-components=1
	install -m 0755 "$tmp/prometheus" /usr/local/bin/prometheus
	install -m 0755 "$tmp/promtool" /usr/local/bin/promtool
	rm -rf "$tmp"
	log "installed"
fi

ensure_user prometheus
install -d -o prometheus -g prometheus -m 0755 /var/lib/prometheus /etc/prometheus

write_file /etc/systemd/system/prometheus.service 0644 <<EOF || true
[Unit]
Description=Prometheus
After=network-online.target
Wants=network-online.target

[Service]
User=prometheus
Group=prometheus
ExecStart=/usr/local/bin/prometheus \\
  --config.file=/etc/prometheus/prometheus.yml \\
  --storage.tsdb.path=/var/lib/prometheus \\
  --storage.tsdb.retention.time=${RETENTION} \\
  --web.listen-address=127.0.0.1:9090
ExecReload=/bin/kill -HUP \$MAINPID
Restart=always
RestartSec=5s
NoNewPrivileges=true
ProtectHome=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

# --------------------------------------------------------------------------
step "Writing Prometheus scrape config"

PROM_YML=/etc/prometheus/prometheus.yml
generate_prom_yml() {
	sed "s|{{AGENT_PORT}}|$AGENT_PORT|g" "$SCRIPT_DIR/prometheus.yml.tmpl"
	if [[ -n "$METRICS_PORT" ]]; then
		cat <<EOF

  - job_name: 'rpc_client'
    metrics_path: '${METRICS_PATH:-/metrics}'
    static_configs:
      - targets: ['127.0.0.1:${METRICS_PORT}']
EOF
	fi
}

# Keep a copy of anything that was already here before replacing it.
if [[ -f "$PROM_YML" ]] && ! generate_prom_yml | cmp -s - "$PROM_YML"; then
	cp -a "$PROM_YML" "$PROM_YML.bak"
	log "existing config backed up to $PROM_YML.bak"
fi
if generate_prom_yml | write_file "$PROM_YML" 0644; then
	chown prometheus:prometheus "$PROM_YML"
	log "scrape config updated"
else
	log "scrape config unchanged"
fi

promtool check config "$PROM_YML" >/dev/null || die "generated prometheus.yml is invalid"

systemctl daemon-reload
systemctl enable --now node_exporter prometheus >/dev/null 2>&1 || true
systemctl restart node_exporter prometheus

# --------------------------------------------------------------------------
step "Checking the node's metrics endpoint"

if [[ -z "$METRICS_PORT" ]]; then
	log "skipped — this chain declares no Prometheus endpoint"
elif curl -fsS --max-time 3 "http://127.0.0.1:${METRICS_PORT}${METRICS_PATH}" -o /dev/null; then
	log "responding"
else
	warn "no response from http://127.0.0.1:${METRICS_PORT}${METRICS_PATH}"
	warn "chain metrics stay empty until the node exposes it"
	warn "geth needs: --metrics --metrics.addr 127.0.0.1 --metrics.port ${METRICS_PORT}"
fi

# --------------------------------------------------------------------------
step "Installing the agent"

ensure_user testmo
install -d -m 0755 /etc/testmo /etc/testmo/chains

# Re-running must not invalidate the key already stored in the dashboard.
API_KEY=""
[[ -f /etc/testmo/agent.yaml ]] && API_KEY="$(yaml_scalar api_key /etc/testmo/agent.yaml)"
if [[ -n "$API_KEY" ]]; then
	log "reusing the existing API key"
else
	API_KEY="$(openssl rand -hex 32)"
	log "generated a new API key"
fi

if ! cmp -s "$AGENT_SRC" /usr/local/bin/testmo-agent; then
	install -m 0755 "$AGENT_SRC" /usr/local/bin/testmo-agent
	RESTART_AGENT=1
	log "binary installed"
fi

if write_file "/etc/testmo/chains/$CHAIN.yaml" 0644 <"$CHAIN_SRC"; then
	RESTART_AGENT=1
	log "chain config installed"
fi

# The allowed_origins block is omitted entirely when no --allow-origin was
# given, so a re-run without the flag produces the same bytes it always did
# and doesn't restart the agent for nothing.
agent_yaml() {
	cat <<EOF
# Managed by testMO setup.sh.
api_key: "$API_KEY"
listen: "127.0.0.1:$AGENT_PORT"
prometheus_url: "http://127.0.0.1:9090"
chain_config: "/etc/testmo/chains/$CHAIN.yaml"
node_id: "$NODE_ID"
EOF
	if [[ ${#ALLOW_ORIGINS[@]} -gt 0 ]]; then
		cat <<'EOF'

# Browser origins allowed to read this API. Anything else gets no
# Access-Control-Allow-Origin header and the browser refuses to hand the
# response to the page. Remove the whole block to allow any origin. Editing
# this needs only `systemctl restart testmo-agent` — nginx never sees it.
allowed_origins:
EOF
		printf '  - "%s"\n' "${ALLOW_ORIGINS[@]}"
	fi
}

if agent_yaml | write_file /etc/testmo/agent.yaml 0640; then
	RESTART_AGENT=1
	log "agent.yaml written"
fi

if [[ ${#ALLOW_ORIGINS[@]} -gt 0 ]]; then
	log "cors: ${ALLOW_ORIGINS[*]}"
else
	log "cors: any origin (pass --allow-origin to restrict)"
fi
chown root:testmo /etc/testmo/agent.yaml

if write_file /etc/systemd/system/testmo-agent.service 0644 <"$SCRIPT_DIR/testmo-agent.service"; then
	RESTART_AGENT=1
	systemctl daemon-reload
fi

systemctl enable testmo-agent >/dev/null 2>&1 || true
if [[ $RESTART_AGENT -eq 1 ]] || ! systemctl is-active --quiet testmo-agent; then
	systemctl restart testmo-agent
	log "agent restarted"
else
	log "agent already running with this config"
fi

# --------------------------------------------------------------------------
step "Configuring nginx"

install -d /etc/nginx/conf.d /etc/nginx/sites-available /etc/nginx/sites-enabled

# Every one of these is an `if`, not `cmd && FLAG=1`: write_file returns 1 for
# "unchanged", and under `set -e` a false && list aborts the whole script.
if write_file /etc/nginx/conf.d/testmo-ratelimit.conf 0644 <"$SCRIPT_DIR/testmo-ratelimit.conf"; then
	RELOAD_NGINX=1
fi

# The plain-HTTP vhost goes in first even when TLS was asked for. It references
# no certificate, so it always parses, and it leaves the API serving while the
# certificate is being issued. If issuance then fails, this is what the box is
# left running rather than a vhost pointing at files that don't exist.
if write_file /etc/nginx/sites-available/testmo 0644 <"$SCRIPT_DIR/testmo-nginx.conf"; then
	RELOAD_NGINX=1
fi
if link_site testmo; then
	RELOAD_NGINX=1
fi

if [[ $RELOAD_NGINX -eq 1 ]]; then
	nginx_reload_or_revert
	RELOAD_NGINX=0
	log "listening on :$API_PORT (http)"
else
	log "listening on :$API_PORT (http), config unchanged"
fi

if [[ -n "$DOMAIN" ]]; then
	# ------------------------------------------------------------------
	step "Obtaining a certificate for $DOMAIN"

	install -d -m 0755 "$ACME_WEBROOT"
	if render testmo-acme.conf.tmpl | write_file /etc/nginx/sites-available/testmo-acme 0644; then
		RELOAD_NGINX=1
	fi
	if link_site testmo-acme; then
		RELOAD_NGINX=1
	fi
	if [[ $RELOAD_NGINX -eq 1 ]]; then
		nginx_reload_or_revert
		RELOAD_NGINX=0
	fi

	# Port 80 has to be open for the challenge, now and at every renewal.
	# Opened before certbot runs, or the first validation fails.
	if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "^Status: active"; then
		ufw allow 80/tcp >/dev/null
		log "ufw: allowed 80/tcp (needed for issue and renewal)"
	else
		log "ufw inactive — ensure 80/tcp is reachable, at install and at renewal"
	fi

	# A renewed certificate is inert until nginx re-reads it. Without this the
	# API keeps serving the old certificate and starts failing the day it
	# expires, months after anyone touched this box. The hook goes in the
	# global deploy directory rather than into this certificate's renewal
	# config, so it survives the certificate being reissued by hand later.
	install -d -m 0755 /etc/letsencrypt/renewal-hooks/deploy
	write_file /etc/letsencrypt/renewal-hooks/deploy/testmo-reload-nginx.sh 0755 <<'EOF' || true
#!/bin/sh
# Installed by testMO setup.sh. Reloads nginx after certbot renews, so the
# fresh certificate is actually served.
systemctl reload nginx
EOF

	if [[ -s "/etc/letsencrypt/live/$DOMAIN/fullchain.pem" ]]; then
		log "certificate already present, leaving it alone"
	else
		if [[ -n "$LE_EMAIL" ]]; then
			EMAIL_ARGS=(--email "$LE_EMAIL")
		else
			EMAIL_ARGS=(--register-unsafely-without-email)
			warn "no --email: nobody gets warned if renewal silently stops working"
		fi

		# --webroot, not --nginx: the nginx plugin rewrites vhosts to inject
		# its own config, and this nginx is also serving the customer's RPC on
		# :443. Nothing here edits a file we didn't write.
		if ! certbot certonly --webroot -w "$ACME_WEBROOT" \
			-d "$DOMAIN" --cert-name "$DOMAIN" \
			--non-interactive --agree-tos --no-eff-email "${EMAIL_ARGS[@]}"; then
			warn "the API is still up on http://$DOMAIN:$API_PORT, but no browser"
			warn "on an https:// page can reach it until this succeeds."
			die "certbot could not issue a certificate for $DOMAIN — check that 80/tcp is open from the internet (cloud firewall included)"
		fi
		log "certificate issued"
	fi

	systemctl enable --now certbot.timer >/dev/null 2>&1 ||
		warn "could not enable certbot.timer — renewals will NOT run automatically"

	# ------------------------------------------------------------------
	step "Enabling TLS on :$API_PORT"

	if render testmo-nginx-tls.conf.tmpl | write_file /etc/nginx/sites-available/testmo 0644; then
		nginx_reload_or_revert
		log "listening on :$API_PORT (https)"
	else
		log "listening on :$API_PORT (https), config unchanged"
	fi
fi

# --------------------------------------------------------------------------
step "Opening the firewall"

if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "^Status: active"; then
	ufw allow "$API_PORT/tcp" >/dev/null
	log "ufw: allowed $API_PORT/tcp"
else
	log "ufw inactive — ensure $API_PORT/tcp is reachable by other means"
fi

# --------------------------------------------------------------------------
step "Verifying"

# With TLS on, --resolve exercises the real certificate and SNI without
# depending on the box being able to route back to its own public IP, which
# plenty of cloud networks won't do.
if [[ -n "$DOMAIN" ]]; then
	BASE_URL="https://$DOMAIN:$API_PORT"
	HEALTH_URL="$BASE_URL/api/v1/health"
	CURL_ARGS=(--resolve "$DOMAIN:$API_PORT:127.0.0.1")
else
	BASE_URL="http://$(hostname -I 2>/dev/null | awk '{print $1}'):$API_PORT"
	HEALTH_URL="http://127.0.0.1:$API_PORT/api/v1/health"
	CURL_ARGS=()
fi

HEALTH=""
for _ in 1 2 3 4 5; do
	if HEALTH="$(curl -fsS --max-time 3 "${CURL_ARGS[@]}" -H "X-TestMO-Key: $API_KEY" \
		"$HEALTH_URL" 2>/dev/null)"; then
		break
	fi
	sleep 1
done

if [[ -n "$HEALTH" ]]; then
	log "$HEALTH"
else
	warn "health check failed against $HEALTH_URL"
	warn "inspect with: journalctl -u testmo-agent -n 50; nginx -t"
fi

cat <<EOF

============================================================
 testMO agent installed

 Base URL: $BASE_URL
 API Key:  $API_KEY

 Store BOTH on this node's Supabase row — the dashboard reads
 the hostname and the key from there, and the key is the only
 credential the API accepts.

 Test from anywhere:
   curl -H "X-TestMO-Key: $API_KEY" \\
     $BASE_URL/api/v1/health

 Logs: journalctl -u testmo-agent -f
EOF

if [[ -n "$DOMAIN" ]]; then
	cat <<EOF

 TLS is on. Renewal is certbot.timer's job and needs 80/tcp
 open from the internet permanently — close it and the cert
 expires in 90 days, taking the dashboard down with it.
   certbot renew --dry-run     # verify renewal actually works
EOF
else
	cat <<EOF

 Plain HTTP: browsers on an https:// dashboard CANNOT call
 this server — every request is blocked as mixed content.
 Re-run with --domain <fqdn> to fix it; the API key survives.
EOF
fi

printf '============================================================\n'
