#!/bin/bash

# MailFS Single Account Test Runner Script
# Runs tests with single-account configuration

export TEST_CONFIG=./pkg/object/mailfs_conf_single.json

echo "=========================================="
echo "MailFS Single Account Test Runner"
echo "=========================================="
echo "Config: $TEST_CONFIG"
echo ""

# Check if config file exists
if [ ! -f "$TEST_CONFIG" ]; then
    echo "❌ Error: Config file not found: $TEST_CONFIG"
    exit 1
fi

echo "Running MailFS Single Account Tests..."
echo ""

# Run the tests
go clean -testcache
go test -v ./pkg/object -run "TestMailFSBasic|TestBlobDataStorage|TestGetOffsetAndLimit"

# Capture exit code
exit_code=$?

echo ""
echo "=========================================="
if [ $exit_code -eq 0 ]; then
    echo "✅ Tests PASSED"
else
    echo "❌ Tests FAILED (exit code: $exit_code)"
fi
echo "=========================================="

exit $exit_code
