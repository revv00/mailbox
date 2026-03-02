#!/bin/bash

# MailFS Multi-Account Replication Test Runner Script
# Runs replication-related tests using the multi-account config file

set -euo pipefail

# Default config (relative to repo root)
TEST_CONFIG_PATH=./pkg/object/mailfs_conf_multi1.json

export TEST_CONFIG="$TEST_CONFIG_PATH"

echo "=========================================="
echo "MailFS Multi-Account Replication Test Runner"
echo "=========================================="
echo "Config: $TEST_CONFIG"
echo ""

# Check if config file exists
if [ ! -f "$TEST_CONFIG" ]; then
    echo "❌ Error: Config file not found: $TEST_CONFIG"
    exit 1
fi

echo "Running replication tests against config: $TEST_CONFIG"
echo ""

# Clean test cache to ensure fresh run
go clean -testcache

# By default run the logic/unit replication tests. If you want to
# exercise real IMAP connectivity (may contact external servers),
# set RUN_REAL_IMAP=1 in your environment to include the integration test.
if [ "${RUN_REAL_IMAP:-0}" = "1" ]; then
    TEST_PATTERN="TestReplicationLogic|TestMailFSBasic|TestRealIMAPConnection"
    echo "Including real IMAP integration test (RUN_REAL_IMAP=1)."
else
    TEST_PATTERN="TestReplicationLogic|TestMailFSBasic"
fi

echo "Test pattern: $TEST_PATTERN"

# Run the tests
go test -v ./pkg/mailfs -run "$TEST_PATTERN"

exit_code=$?

echo ""
echo "=========================================="
if [ $exit_code -eq 0 ]; then
    echo "✅ Replication Tests PASSED"
else
    echo "❌ Replication Tests FAILED (exit code: $exit_code)"
fi
echo "=========================================="

exit $exit_code
