#!/bin/bash

# MailFS Setup Script for JuiceFS
# This script helps set up JuiceFS with MailFS (email-based object storage)

set -e

echo "=========================================="
echo "JuiceFS MailFS Setup"
echo "=========================================="
echo ""

# Check dependencies
echo "[1/5] Checking dependencies..."

if ! command -v juicefs &> /dev/null; then
    echo "ERROR: juicefs not found. Please install JuiceFS first."
    exit 1
fi

if ! command -v redis-server &> /dev/null; then
    echo "WARNING: redis-server not found. You'll need Redis running for the metadata store."
fi

if ! command -v sqlite3 &> /dev/null; then
    echo "WARNING: sqlite3 not found. MailFS uses SQLite for local metadata."
fi

echo "✓ Dependencies check complete"
echo ""

# Configuration
REDIS_HOST="${REDIS_HOST:-localhost}"
REDIS_PORT="${REDIS_PORT:-6379}"
REDIS_DB="${REDIS_DB:-1}"
REDIS_URL="redis://${REDIS_HOST}:${REDIS_PORT}/${REDIS_DB}"
FS_NAME="${1:-mymailfs}"
MOUNT_POINT="${2:-/mnt/mailfs}"
CONFIG_FILE="${3:-mailfs-accounts.json}"

echo "[2/5] Configuration"
echo "  Filesystem name: $FS_NAME"
echo "  Mount point: $MOUNT_POINT"
echo "  Config file: $CONFIG_FILE"
echo "  Redis URL: $REDIS_URL"
echo ""

# Validate configuration file
if [ ! -f "$CONFIG_FILE" ]; then
    echo "ERROR: Configuration file not found: $CONFIG_FILE"
    echo ""
    echo "Please create a mailfs-accounts.json file with email accounts."
    echo "See MAILFS_README.md for examples."
    exit 1
fi

echo "[3/5] Validating email accounts..."
# Simple JSON validation
if ! python3 -m json.tool "$CONFIG_FILE" > /dev/null 2>&1; then
    echo "ERROR: Invalid JSON in $CONFIG_FILE"
    exit 1
fi

# Count accounts
ACCOUNT_COUNT=$(python3 -c "import json; print(len(json.load(open('$CONFIG_FILE'))))")
if [ "$ACCOUNT_COUNT" -lt 1 ]; then
    echo "ERROR: No email accounts found in configuration"
    exit 1
fi
if [ "$ACCOUNT_COUNT" -gt 10 ]; then
    echo "WARNING: More than 10 accounts not recommended"
fi
echo "✓ Found $ACCOUNT_COUNT email accounts"
echo ""

# Create mount point
echo "[4/5] Preparing mount point..."
if [ ! -d "$MOUNT_POINT" ]; then
    echo "Creating directory: $MOUNT_POINT"
    mkdir -p "$MOUNT_POINT"
fi
echo "✓ Mount point ready: $MOUNT_POINT"
echo ""

# Format JuiceFS
echo "[5/5] Formatting JuiceFS..."
echo ""
echo "Running: juicefs format --storage mailfs --bucket $CONFIG_FILE $REDIS_URL $FS_NAME"
echo ""

if juicefs format --storage mailfs --bucket "$CONFIG_FILE" "$REDIS_URL" "$FS_NAME"; then
    echo ""
    echo "✓ Filesystem formatted successfully!"
    echo ""
    echo "Next steps:"
    echo "  1. Start Redis (if needed):"
    echo "     redis-server --daemonize yes"
    echo ""
    echo "  2. Mount the filesystem:"
    echo "     juicefs mount $REDIS_URL $MOUNT_POINT"
    echo ""
    echo "  3. Use the filesystem:"
    echo "     echo 'test' > $MOUNT_POINT/test.txt"
    echo "     cat $MOUNT_POINT/test.txt"
    echo ""
    echo "  4. Check status:"
    echo "     juicefs info $REDIS_URL"
    echo ""
else
    echo ""
    echo "ERROR: Failed to format filesystem"
    exit 1
fi
