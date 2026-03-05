#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

REPO_ROOT=$(pwd)
echo -e "${GREEN}Building mbox CLI...${NC}"
go build -o mbox ./cmd/mbox
MBOX_BIN="$REPO_ROOT/mbox"

echo "Waiting for mail services..."
sleep 20

# Setup test environment
TEST_ROOT=$(mktemp -d)
trap 'chmod -R +w "$TEST_ROOT" && rm -rf "$TEST_ROOT"' EXIT

echo "Test Workspace: $TEST_ROOT"
export HOME="$TEST_ROOT"
mkdir -p "$HOME/.mbox"

# Generate accounts.json for CI
# ... (rest of the accounts.json generation)
cat <<EOF > "$HOME/.mbox/accounts.json"
{
  "accounts": [
    {
      "email": "user1@provider1.com",
      "password": "password",
      "imapHost": "127.0.0.1:3143",
      "smtpHost": "127.0.0.1:3025"
    },
    {
      "email": "user2@provider1.com",
      "password": "password",
      "imapHost": "127.0.0.1:3143",
      "smtpHost": "127.0.0.1:3025"
    },
    {
      "email": "user1@provider2.com",
      "password": "password",
      "imapHost": "127.0.0.1:3144",
      "smtpHost": "127.0.0.1:3026"
    },
    {
      "email": "user2@provider2.com",
      "password": "password",
      "imapHost": "127.0.0.1:3144",
      "smtpHost": "127.0.0.1:3026"
    }
  ],
  "replication": 2,
  "subject_prefix": "CI-TEST:",
  "remove_sent": true
}
EOF

export MAILFS_SKIP_TLS_VERIFY=1
export MBOX_MASTER_PASSWORD=master_password
export MBOX_ARCHIVE_PASSWORD=archive_password

# Create a test directory with a "large" file to test chunking/parallelism
mkdir -p "$TEST_ROOT/test_data"
dd if=/dev/urandom of="$TEST_ROOT/test_data/file1.bin" bs=1M count=20
dd if=/dev/urandom of="$TEST_ROOT/test_data/file2.bin" bs=1M count=5
md5sum "$TEST_ROOT/test_data/file1.bin" | awk '{print $1}' > "$TEST_ROOT/orig1.md5"
md5sum "$TEST_ROOT/test_data/file2.bin" | awk '{print $1}' > "$TEST_ROOT/orig2.md5"

echo -e "\n${GREEN}=== Testing PUT (2x Replication) ===${NC}"
cd "$TEST_ROOT"
"$MBOX_BIN" put -p "$MBOX_ARCHIVE_PASSWORD" "$TEST_ROOT/test_data"

echo -e "\n${GREEN}=== Testing GET (Download and Verify) ===${NC}"
RESTORE_DIR="$TEST_ROOT/restore"
mkdir -p "$RESTORE_DIR"
cp "test_data.mbox" "$RESTORE_DIR/"
cd "$RESTORE_DIR"

"$MBOX_BIN" get -p "$MBOX_ARCHIVE_PASSWORD" "test_data.mbox"

md5sum "test_data/file1.bin" | awk '{print $1}' > "$TEST_ROOT/new1.md5"
md5sum "test_data/file2.bin" | awk '{print $1}' > "$TEST_ROOT/new2.md5"

if diff "$TEST_ROOT/orig1.md5" "$TEST_ROOT/new1.md5" && diff "$TEST_ROOT/orig2.md5" "$TEST_ROOT/new2.md5"; then
    echo -e "${GREEN}SUCCESS: Files restored perfectly.${NC}"
else
    echo -e "${RED}FAILURE: Checksum mismatch!${NC}"
    exit 1
fi

echo -e "\n${GREEN}=== Testing Resume Logic (Simulated Failure) ===${NC}"
cd "$REPO_ROOT"
rm -rf ~/.mbox/state
# No need to remove resume_test.mbox if we use test_data.mbox consistently

# Patch to fail after 10MB
perl -i -pe 's/wErr := c\.vfs\.Write/if offset >= 10 * 1024 * 1024 { panic("simulated failure") }\n\t\t\t\twErr := c\.vfs\.Write/' pkg/mbox/client.go
go build -o mbox ./cmd/mbox

echo "First attempt (should fail)"
cd "$TEST_ROOT"
"$MBOX_BIN" stash --remove-sent=false -p "$MBOX_ARCHIVE_PASSWORD" "$TEST_ROOT/test_data" || echo "failed as expected"

echo "Restoring code and resuming"
cd "$REPO_ROOT"
git checkout pkg/mbox/client.go
go build -o mbox ./cmd/mbox

cd "$TEST_ROOT"
"$MBOX_BIN" stash --remove-sent=false -p "$MBOX_ARCHIVE_PASSWORD" "$TEST_ROOT/test_data"

echo "Verifying resumed upload"
mkdir -p "$TEST_ROOT/restore_resume"
cp "test_data.mbox" "$TEST_ROOT/restore_resume/"
cd "$TEST_ROOT/restore_resume"
"$MBOX_BIN" get -p "$MBOX_ARCHIVE_PASSWORD" "test_data.mbox"

md5sum "test_data/file1.bin" | awk '{print $1}' > "$TEST_ROOT/resume1.md5"
if diff "$TEST_ROOT/orig1.md5" "$TEST_ROOT/resume1.md5"; then
    echo -e "${GREEN}SUCCESS: Resumed upload verified.${NC}"
else
    echo -e "${RED}FAILURE: Resume verification failed.${NC}"
    exit 1
fi

echo -e "\n${GREEN}CI Regression Tests Completed Successfully!${NC}"
