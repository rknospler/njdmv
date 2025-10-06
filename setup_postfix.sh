#!/bin/bash

# Gmail Postfix Setup Script for macOS
# This script configures Postfix to send emails via Gmail

echo "=== Gmail Postfix Setup for macOS ==="
echo ""
echo "Before running this script, you need:"
echo "1. Your Gmail address"
echo "2. A Gmail App Password (from https://myaccount.google.com/apppasswords)"
echo ""
read -p "Enter your Gmail address: " GMAIL_USER
read -sp "Enter your Gmail App Password (16 chars): " GMAIL_PASS
echo ""

# Create SASL password file
echo "Creating SASL password file..."
echo "smtp.gmail.com:587 ${GMAIL_USER}:${GMAIL_PASS}" | sudo tee /etc/postfix/sasl_passwd > /dev/null

# Secure the password file
echo "Securing password file..."
sudo chmod 600 /etc/postfix/sasl_passwd
sudo postmap /etc/postfix/sasl_passwd

# Backup original Postfix config
if [ ! -f /etc/postfix/main.cf.backup ]; then
    echo "Backing up original Postfix config..."
    sudo cp /etc/postfix/main.cf /etc/postfix/main.cf.backup
fi

# Configure Postfix main.cf
echo "Configuring Postfix..."
sudo tee -a /etc/postfix/main.cf > /dev/null <<EOF

# Gmail SMTP Relay Configuration
relayhost = smtp.gmail.com:587
smtp_sasl_auth_enable = yes
smtp_sasl_password_maps = hash:/etc/postfix/sasl_passwd
smtp_sasl_security_options = noanonymous
smtp_sasl_mechanism_filter = plain
smtp_use_tls = yes
smtp_tls_security_level = encrypt
smtp_tls_CAfile = /etc/ssl/cert.pem
EOF

# Reload Postfix
echo "Reloading Postfix..."
sudo postfix reload

echo ""
echo "✅ Setup complete!"
echo ""
echo "Test by running:"
echo "  echo 'Test message' | mail -s 'Test' 9736705400@tmomail.net"
echo ""
