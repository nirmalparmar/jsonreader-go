#!/usr/bin/env bash
# One-shot deploy of common-service on a Linux VM.
# Supports Ubuntu/Debian (apt, ufw) and Oracle Linux / RHEL / Rocky / Alma 9
# (dnf, firewalld, SELinux). Works behind Cloudflare.
#
#   sudo ./deploy/setup.sh <api-domain> <email-for-letsencrypt>
#   sudo ./deploy/setup.sh api.jsonreader.com you@example.com
#
# Safe to re-run: it rebuilds and restarts the service (use it for updates)
# and skips steps that are already done (packages, certificate).
set -euo pipefail

DOMAIN="${1:-}"
EMAIL="${2:-}"
APP_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SITE="/etc/nginx/conf.d/${DOMAIN}.conf"
REALIP="/etc/nginx/conf.d/00-cloudflare-realip.conf"

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
warn() { printf '\033[1;33mwarning:\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31merror:\033[0m %s\n' "$*" >&2; exit 1; }

[[ -n "$DOMAIN" && -n "$EMAIL" ]] || die "usage: sudo $0 <api-domain> <email>"
[[ "$(id -u)" -eq 0 ]] || die "run with sudo"
[[ -f "$APP_DIR/docker-compose.yml" ]] || die "run this from inside the common-service directory"
cd "$APP_DIR"

# shellcheck disable=SC1091
. /etc/os-release
case " ${ID:-} ${ID_LIKE:-} " in
  *" debian "*|*" ubuntu "*)            FAMILY=debian ;;
  *" rhel "*|*" fedora "*|*" centos "*) FAMILY=rhel ;;
  *) die "unsupported OS: ${PRETTY_NAME:-unknown} (need Ubuntu/Debian or an EL9 distro)" ;;
esac

# ---------------------------------------------------------------------------
step "Checking the VM (${PRETTY_NAME:-unknown})"
[[ "$(stat -fc %T /sys/fs/cgroup)" == "cgroup2fs" ]] || die "cgroup v2 is required"
[[ "$(sysctl -n user.max_user_namespaces 2>/dev/null || echo 0)" -gt 0 ]] \
  || die "user namespaces are disabled (user.max_user_namespaces=0); the sandbox needs them"
if systemd-detect-virt -c -q 2>/dev/null; then
  die "this looks like a container-based VPS ($(systemd-detect-virt -c)); nsjail needs a real VM (KVM/Xen/Hyper-V)"
fi
mem_mb=$(awk '/MemTotal/ {print int($2/1024)}' /proc/meminfo)
disk_gb=$(df -BG --output=avail /var/lib 2>/dev/null | tail -1 | tr -dc 0-9)
echo "cpus=$(nproc) memory=${mem_mb}MB free_disk=${disk_gb}GB arch=$(uname -m)"
(( mem_mb >= 3500 )) || warn "less than 4 GB RAM; consider a bigger VM or fewer LANGS"
(( ${disk_gb:-0} >= 10 )) || warn "under 10 GB free disk; the build needs ~6-8 GB (reduce LANGS or free space)"
if ss -ltnH 'sport = :8080' | grep -q . && ! docker ps --format '{{.Ports}}' 2>/dev/null | grep -q '127.0.0.1:8080'; then
  die "port 8080 is already in use by another program"
fi

# ---------------------------------------------------------------------------
step "Installing Docker, nginx, certbot"
if [[ "$FAMILY" == debian ]]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq nginx certbot python3-certbot-nginx ufw curl dnsutils iproute2 >/dev/null
  if ! command -v docker >/dev/null; then
    curl -fsSL https://get.docker.com | sh
  fi
else
  dnf -y -q install dnf-plugins-core curl bind-utils iproute
  if ! rpm -q epel-release oracle-epel-release-el9 >/dev/null 2>&1; then
    if [[ "${ID}" == "ol" ]]; then
      dnf -y -q install "oracle-epel-release-el${VERSION_ID%%.*}"
    else
      dnf -y -q install epel-release
    fi
  fi
  dnf -y -q install nginx certbot python3-certbot-nginx
  if ! command -v docker >/dev/null; then
    dnf config-manager --add-repo https://download.docker.com/linux/rhel/docker-ce.repo
    dnf -y -q install --allowerasing docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
  fi
  systemctl enable --now certbot-renew.timer 2>/dev/null || true
fi
systemctl enable --now docker
systemctl enable --now nginx

# ---------------------------------------------------------------------------
step "Firewall (SSH + HTTP/HTTPS only)"
WEB_RULE=(-p tcp -m state --state NEW -m multiport --dports "80,443" -j ACCEPT)
# Oracle Cloud images end the INPUT chain with a catch-all REJECT that sits
# before anything ufw adds, so 80/443 must be allowed directly above it.
open_web_before_reject() {
  local cmd=$1 file=$2 n
  command -v "$cmd" >/dev/null || return 1
  "$cmd" -L INPUT -n | awk '$1=="REJECT"' | grep -q . || return 1
  while "$cmd" -D INPUT "${WEB_RULE[@]}" 2>/dev/null; do :; done   # drop misplaced copies
  n=$("$cmd" -L INPUT -n --line-numbers | awk '$2=="REJECT" {print $1; exit}')
  "$cmd" -I INPUT "$n" "${WEB_RULE[@]}"
  # Persist by editing the boot rules file (not netfilter-persistent save,
  # which would also freeze Docker's and ufw's runtime chains into it).
  if [[ -f "$file" ]] && ! grep -q -- '--dports 80,443 -j ACCEPT' "$file"; then
    sed -i "0,/^-A INPUT .*-j REJECT/s//-A INPUT ${WEB_RULE[*]}\n&/" "$file"
  fi
  return 0
}
if open_web_before_reject iptables /etc/iptables/rules.v4; then
  open_web_before_reject ip6tables /etc/iptables/rules.v6 || true
  echo "opened 80/443 above the image's REJECT rule:"
  iptables -L INPUT -n --line-numbers | sed -n '1,9p'
elif systemctl is-active -q firewalld 2>/dev/null; then
  firewall-cmd -q --permanent --add-service=ssh --add-service=http --add-service=https
  firewall-cmd -q --reload
  echo "firewalld services: $(firewall-cmd --list-services)"
elif command -v ufw >/dev/null; then
  ufw allow OpenSSH >/dev/null
  ufw allow 'Nginx Full' >/dev/null
  ufw --force enable >/dev/null
  ufw status | sed -n '1,8p'
else
  warn "no firewalld/ufw found; make sure only ports 22, 80 and 443 are reachable"
fi
echo "note: cloud providers also have a network firewall (e.g. Oracle Cloud VCN security list,"
echo "      AWS security group) - it must allow inbound TCP 80 and 443."

if command -v getenforce >/dev/null && [[ "$(getenforce)" != "Disabled" ]]; then
  # SELinux blocks nginx from opening upstream connections by default.
  setsebool -P httpd_can_network_connect 1
  echo "SELinux: allowed nginx to proxy to the API"
fi

# ---------------------------------------------------------------------------
step "Configuration"
if [[ ! -f .env ]]; then
  cp .env.example .env
  sed -i "s/^COMPILER_MAX_CONCURRENT=.*/COMPILER_MAX_CONCURRENT=$(nproc)/" .env
  chmod 600 .env
  echo "created .env (edit ALLOWED_ORIGINS there if your site is not jsonreader.com)"
else
  echo ".env already exists; keeping it"
fi
grep -E '^(ALLOWED_ORIGINS|TRUSTED_PROXIES|COMPILER_MAX_CONCURRENT)=' .env

# Size the container's memory cap to the VM, leaving ~1 GB for the OS + nginx.
limit_mb=$(( mem_mb > 2048 ? mem_mb - 1024 : mem_mb * 3 / 4 ))
cat > docker-compose.override.yml <<EOF
# Generated by deploy/setup.sh for this VM (${mem_mb} MB RAM).
services:
  api:
    mem_limit: ${limit_mb}m
EOF
echo "container memory limit: ${limit_mb} MB"

# ---------------------------------------------------------------------------
step "Building and starting the service (first build takes ~10-20 min)"
docker compose up -d --build
cid=$(docker compose ps -q api)
health=starting
for _ in $(seq 1 60); do
  health=$(docker inspect -f '{{.State.Health.Status}}' "$cid" 2>/dev/null || echo starting)
  [[ "$health" == "healthy" ]] && break
  sleep 3
done
[[ "$health" == "healthy" ]] || { docker compose logs --tail 50 api; die "service did not become healthy"; }
curl -fsS http://127.0.0.1:8080/healthz && echo
if docker compose logs api 2>&1 | grep -q "entrypoint:"; then
  docker compose logs api 2>&1 | grep "entrypoint:"
  warn "sandbox fell back to weaker limits; see the messages above"
fi

# ---------------------------------------------------------------------------
step "nginx + HTTPS for ${DOMAIN}"
# Trust Cloudflare's CF-Connecting-IP (only from Cloudflare's own ranges) so
# rate limits apply per visitor instead of per Cloudflare edge server.
if cf4=$(curl -fsS --max-time 10 https://www.cloudflare.com/ips-v4) && cf6=$(curl -fsS --max-time 10 https://www.cloudflare.com/ips-v6); then
  {
    echo "# Generated by deploy/setup.sh - real client IPs behind Cloudflare."
    for r in $cf4 $cf6; do echo "set_real_ip_from $r;"; done
    echo "real_ip_header CF-Connecting-IP;"
  } > "$REALIP"
else
  warn "could not download Cloudflare IP ranges; skipping real-IP config"
fi

behind_cf=false
if curl -fsSI --max-time 8 "http://${DOMAIN}/" 2>/dev/null | grep -qi '^server: cloudflare'; then
  behind_cf=true
  echo "${DOMAIN} is proxied by Cloudflare"
else
  public_ip=$(curl -fsS --max-time 5 https://api.ipify.org || true)
  dns_ip=$(dig +short A "$DOMAIN" | tail -1 || true)
  if [[ -n "$public_ip" && "$dns_ip" != "$public_ip" ]]; then
    warn "${DOMAIN} resolves to '${dns_ip:-nothing}' but this VM is ${public_ip}; certbot will fail until DNS points here"
  fi
fi

for f in /etc/nginx/sites-enabled/* /etc/nginx/conf.d/*.conf; do
  [[ -e "$f" ]] || continue
  [[ "$(readlink -f "$f")" == "$SITE" ]] && continue
  if grep -qE "server_name[^;]*\b${DOMAIN//./\\.}\b" "$f"; then
    die "$f already serves ${DOMAIN} (old API?). Remove it, then re-run: sudo rm $f"
  fi
done

if [[ ! -f "/etc/letsencrypt/live/${DOMAIN}/fullchain.pem" ]]; then
  # Temporary plain-HTTP site so Let's Encrypt can verify the domain.
  cat > "$SITE" <<EOF
server { listen 80; listen [::]:80; server_name ${DOMAIN}; location / { return 200 "ok"; } }
EOF
  nginx -t -q && systemctl reload nginx
  if ! certbot certonly --nginx -d "$DOMAIN" -m "$EMAIL" --agree-tos -n \
        --deploy-hook "systemctl reload nginx"; then
    rm -f "$SITE"; systemctl reload nginx
    if $behind_cf; then
      die "certificate request failed. In Cloudflare set the '${DOMAIN%%.*}' DNS record to 'DNS only' (grey cloud), wait a minute, re-run, then switch it back to 'Proxied'."
    fi
    die "certificate request failed. Check that ${DOMAIN} points to this VM and that port 80 is open in the cloud firewall."
  fi
else
  echo "certificate already present"
fi

sed "s/api\.jsonreader\.com/${DOMAIN}/g" deploy/nginx.conf > "$SITE"
nginx -t -q && systemctl reload nginx

# ---------------------------------------------------------------------------
step "Smoke test over HTTPS"
origin=$(grep '^ALLOWED_ORIGINS=' .env | cut -d= -f2 | cut -d, -f1)
if ! curl -fsS --max-time 15 "https://${DOMAIN}/healthz"; then
  warn "https://${DOMAIN} is not reachable from here. Check the cloud firewall (TCP 443) and, with Cloudflare, SSL/TLS mode 'Full (strict)'."
else
  echo
  curl -fsS "https://${DOMAIN}/v1/languages" | grep -o '"id":"[a-z]*"' | cut -d'"' -f4 | tr '\n' ' '; echo
  out=$(curl -fsSN -X POST "https://${DOMAIN}/v1/run/python" \
    -H 'Content-Type: application/json' -H "Origin: ${origin}" \
    -d '{"code":"import socket\ntry:\n socket.create_connection((\"1.1.1.1\",80),2); print(\"NET-OPEN\")\nexcept Exception: print(\"sandbox-ok\")"}' || true)
  case "$out" in
    *sandbox-ok*) echo "code execution: ok (sandbox has no network)";;
    *NET-OPEN*)   die "sandboxed code reached the internet; do not go live";;
    *)            warn "code execution test failed:"; echo "$out" | tail -5;;
  esac
fi

step "Done"
cat <<EOF
API:      https://${DOMAIN}
Logs:     cd ${APP_DIR} && sudo docker compose logs -f api
Update:   copy new code here, then: sudo ./deploy/setup.sh ${DOMAIN} ${EMAIL}
Backup:   sudo docker run --rm -v common-service_data:/data -v "\$PWD":/b busybox tar czf /b/rooms-backup.tgz /data
EOF
if $behind_cf; then
  echo "Cloudflare: set SSL/TLS encryption mode to 'Full (strict)'."
fi
