#!/bin/bash

# Postfix SMTP Relay Setup Script for macOS
# Configures Postfix to relay mail through any SMTP provider (Gmail, Outlook, etc.)
#
# Usage:
#   ./setup_postfix.sh                          # interactive prompts
#   ./setup_postfix.sh --smtp-host smtp.gmail.com --smtp-port 587 \
#       --smtp-user you@gmail.com --smtp-pass 'xxxx xxxx xxxx xxxx' \
#       --test-addr 5551234567@tmomail.net

set -euo pipefail

# ── Load .env if present ──────────────────────────────────────────────────────
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
if [[ -f "${SCRIPT_DIR}/.env" ]]; then
    # shellcheck disable=SC1091
    source "${SCRIPT_DIR}/.env"
fi

# ── Defaults (overridden by .env or CLI flags) ───────────────────────────────
SMTP_HOST="${SMTP_HOST:-}"
SMTP_PORT="${SMTP_PORT:-587}"
SMTP_USER="${SMTP_USER:-}"
SMTP_PASS="${SMTP_PASS:-}"
TEST_ADDR="${TEST_ADDR:-}"
TLS_CA="${TLS_CA:-/etc/ssl/cert.pem}"

# ── Parse CLI flags ──────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
    case "$1" in
        --smtp-host)  SMTP_HOST="$2"; shift 2 ;;
        --smtp-port)  SMTP_PORT="$2"; shift 2 ;;
        --smtp-user)  SMTP_USER="$2"; shift 2 ;;
        --smtp-pass)  SMTP_PASS="$2"; shift 2 ;;
        --test-addr)  TEST_ADDR="$2"; shift 2 ;;
        --tls-ca)     TLS_CA="$2";    shift 2 ;;
        -h|--help)
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --smtp-host HOST   SMTP relay hostname (e.g. smtp.gmail.com)"
            echo "  --smtp-port PORT   SMTP relay port (default: 587)"
            echo "  --smtp-user USER   SMTP username / email address"
            echo "  --smtp-pass PASS   SMTP password or app password"
            echo "  --test-addr ADDR   Optional address to send a test email to"
            echo "  --tls-ca PATH      TLS CA bundle path (default: /etc/ssl/cert.pem)"
            echo "  -h, --help         Show this help message"
            echo ""
            echo "Common SMTP hosts:"
            echo "  Gmail:     smtp.gmail.com     (port 587, requires App Password)"
            echo "  Outlook:   smtp.office365.com (port 587)"
            echo "  Yahoo:     smtp.mail.yahoo.com (port 587)"
            echo "  iCloud:    smtp.mail.me.com   (port 587, requires App Password)"
            exit 0
            ;;
        *) echo "Unknown option: $1"; exit 1 ;;
    esac
done

# ── Interactive prompts for missing values ────────────────────────────────────
echo "=== Postfix SMTP Relay Setup for macOS ==="
echo ""

if [[ -z "$SMTP_HOST" ]]; then
    read -p "SMTP host (e.g. smtp.gmail.com): " SMTP_HOST
fi
if [[ -z "$SMTP_HOST" ]]; then
    echo "Error: SMTP host is required." >&2
    exit 1
fi

if [[ "$SMTP_PORT" == "587" ]]; then
    read -p "SMTP port [587]: " input_port
    SMTP_PORT="${input_port:-587}"
fi

if [[ -z "$SMTP_USER" ]]; then
    read -p "SMTP username (email address): " SMTP_USER
fi
if [[ -z "$SMTP_USER" ]]; then
    echo "Error: SMTP username is required." >&2
    exit 1
fi

if [[ -z "$SMTP_PASS" ]]; then
    read -sp "SMTP password / app password: " SMTP_PASS
    echo ""
fi
if [[ -z "$SMTP_PASS" ]]; then
    echo "Error: SMTP password is required." >&2
    exit 1
fi

RELAY="${SMTP_HOST}:${SMTP_PORT}"

# ── Create SASL password file ─────────────────────────────────────────────────
echo "Creating SASL password file..."
echo "${RELAY} ${SMTP_USER}:${SMTP_PASS}" | sudo tee /etc/postfix/sasl_passwd > /dev/null
sudo chmod 600 /etc/postfix/sasl_passwd
sudo postmap /etc/postfix/sasl_passwd

# ── Backup original Postfix config ───────────────────────────────────────────
if [[ ! -f /etc/postfix/main.cf.backup ]]; then
    echo "Backing up original Postfix config..."
    sudo cp /etc/postfix/main.cf /etc/postfix/main.cf.backup
fi

# ── Remove any previous relay block we wrote ─────────────────────────────────
sudo sed -i '' '/^# SMTP Relay Configuration (setup_postfix.sh)/,/^$/d' /etc/postfix/main.cf 2>/dev/null || true

# ── Write new relay configuration ────────────────────────────────────────────
echo "Configuring Postfix for ${RELAY}..."
sudo tee -a /etc/postfix/main.cf > /dev/null <<EOF

# SMTP Relay Configuration (setup_postfix.sh)
relayhost = ${RELAY}
smtp_sasl_auth_enable = yes
smtp_sasl_password_maps = hash:/etc/postfix/sasl_passwd
smtp_sasl_security_options = noanonymous
smtp_sasl_mechanism_filter = plain
smtp_use_tls = yes
smtp_tls_security_level = encrypt
smtp_tls_CAfile = ${TLS_CA}

EOF

# ── Reload Postfix ───────────────────────────────────────────────────────────
echo "Reloading Postfix..."
sudo postfix reload 2>/dev/null || sudo postfix start

echo ""
echo "✅ Setup complete! Relay: ${RELAY} (user: ${SMTP_USER})"

# ── Optional test email ──────────────────────────────────────────────────────
if [[ -z "$TEST_ADDR" ]]; then
    read -p "Send a test email? Enter address (or leave blank to skip): " TEST_ADDR
fi

if [[ -n "$TEST_ADDR" ]]; then
    echo "Sending test email to ${TEST_ADDR}..."
    echo "Test from setup_postfix.sh at $(date)" | mail -s "Postfix Test" "$TEST_ADDR"
    echo "📧 Test email sent. Check ${TEST_ADDR} (may take a minute)."
fi
