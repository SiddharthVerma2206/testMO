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

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

CHAIN="geth"
METRICS_PORT=""
METRICS_PATH=""
NODE_ID=""
RESTART_AGENT=0

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
EOF
}

while [[ $# -gt 0 ]]; do
	case "$1" in
		--chain)        CHAIN="${2:-}";        shift 2 ;;
		--metrics-port) METRICS_PORT="${2:-}"; shift 2 ;;
		--metrics-path) METRICS_PATH="${2:-}"; shift 2 ;;
		--node-id)      NODE_ID="${2:-}";      shift 2 ;;
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

if write_file /etc/testmo/agent.yaml 0640 <<EOF; then
# Managed by testMO setup.sh.
api_key: "$API_KEY"
listen: "127.0.0.1:$AGENT_PORT"
prometheus_url: "http://127.0.0.1:9090"
chain_config: "/etc/testmo/chains/$CHAIN.yaml"
node_id: "$NODE_ID"
EOF
	RESTART_AGENT=1
	log "agent.yaml written"
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
write_file /etc/nginx/conf.d/testmo-ratelimit.conf 0644 <"$SCRIPT_DIR/testmo-ratelimit.conf" || true
write_file /etc/nginx/sites-available/testmo 0644 <"$SCRIPT_DIR/testmo-nginx.conf" || true
ln -sfn ../sites-available/testmo /etc/nginx/sites-enabled/testmo

# The node's own RPC is served by this nginx. Never reload a config that
# doesn't parse — back the change out instead.
if ! nginx -t >/dev/null 2>&1; then
	rm -f /etc/nginx/sites-enabled/testmo /etc/nginx/conf.d/testmo-ratelimit.conf
	nginx -t || true
	die "nginx rejected the config — reverted, your existing sites are untouched"
fi
systemctl reload nginx
log "listening on :$API_PORT"

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

HEALTH=""
for _ in 1 2 3 4 5; do
	if HEALTH="$(curl -fsS --max-time 3 -H "X-TestMO-Key: $API_KEY" \
		"http://127.0.0.1:$API_PORT/api/v1/health" 2>/dev/null)"; then
		break
	fi
	sleep 1
done

if [[ -n "$HEALTH" ]]; then
	log "$HEALTH"
else
	warn "health check failed — inspect with: journalctl -u testmo-agent -n 50"
fi

cat <<EOF

============================================================
 testMO agent installed

 API Key: $API_KEY

 Store this alongside the node's record in your dashboard —
 it is the only credential the API accepts.

 Test from anywhere:
   curl -H "X-TestMO-Key: $API_KEY" \\
     http://$(hostname -I 2>/dev/null | awk '{print $1}'):$API_PORT/api/v1/health

 Logs: journalctl -u testmo-agent -f
============================================================
EOF
