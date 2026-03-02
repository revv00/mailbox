#!/bin/bash

# MailFS Delete Sent Message Test Runner Script
# Runs specialized tests for deleteSentMessage functionality

# export TEST_CONFIG=./pkg/object/mailfs_conf_single.json
export TEST_CONFIG=./pkg/object/mailfs_conf_single163.json

echo "=========================================="
echo "MailFS Delete Sent Message Test Runner"
echo "=========================================="
echo "Config: $TEST_CONFIG"
echo ""

# Check if config file exists
if [ ! -f "$TEST_CONFIG" ]; then
    echo "❌ Error: Config file not found: $TEST_CONFIG"
    exit 1
fi

echo "--- STEP 1: Direct deletion test ---"
go test -v ./pkg/mailfs -run TestDeleteSentMessage -count=1
res1=$?

echo ""
echo "--- STEP 2: Put/Stash auto-deletion test ---"
go test -v ./pkg/mailfs -run TestPutWithRemoveSent -count=1
res2=$?

echo ""
echo "=========================================="
if [ $res1 -eq 0 ] && [ $res2 -eq 0 ]; then
    echo "✅ All Tests PASSED"
    exit_code=0
else
    echo "❌ Some Tests FAILED"
    echo "Step 1: $res1"
    echo "Step 2: $res2"
    exit_code=1
fi
echo "=========================================="

exit $exit_code
