#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

echo -e "${GREEN}Building mbox CLI...${NC}"
go build -o mbox ./cmd/mbox
MBOX_BIN=$(realpath ./mbox)

# Setup test environment
TEST_ROOT=$(mktemp -d)
trap 'rm -rf "$TEST_ROOT"' EXIT

echo "Test Workspace: $TEST_ROOT"
export HOME="$TEST_ROOT"
mkdir -p "$HOME/.mbox"

# Generate accounts.json for CI
# Service 1: provider1.com (SMTP 3025, IMAP 3143)
# Service 2: provider2.com (SMTP 3026, IMAP 3144)
cat <<EOF > "$HOME/.mbox/accounts.json"
{
  "accounts": [
    {
      "email": "user1@provider1.com",
      "password": "password",
      "imapHost": "localhost:3143",
      "smtpHost": "localhost:3025"
    },
    {
      "email": "user2@provider1.com",
      "password": "password",
      "imapHost": "localhost:3143",
      "smtpHost": "localhost:3025"
    },
    {
      "email": "user1@provider2.com",
      "password": "password",
      "imapHost": "localhost:3144",
      "smtpHost": "localhost:3026"
    },
    {
      "email": "user2@provider2.com",
      "password": "password",
      "imapHost": "localhost:3144",
      "smtpHost": "localhost:3026"
    }
  ],
  "replication": 2,
  "subject_prefix": "CI-TEST:",
  "remove_sent": true
}
EOF

export MBOX_MASTER_PASSWORD=master_password
export MBOX_ARCHIVE_PASSWORD=archive_password

# Create a test directory with a "large" file to test chunking/parallelism
mkdir -p "$TEST_ROOT/test_data"
dd if=/dev/urandom of="$TEST_ROOT/test_data/file1.bin" bs=1M count=20
dd if=/dev/urandom of="$TEST_ROOT/test_data/file2.bin" bs=1M count=5
md5sum "$TEST_ROOT/test_data/file1.bin" > "$TEST_ROOT/orig1.md5"
md5sum "$TEST_ROOT/test_data/file2.bin" > "$TEST_ROOT/orig2.md5"

echo -e "\n${GREEN}=== Testing PUT (2x Replication) ===${NC}"
"$MBOX_BIN" put -p "$MBOX_ARCHIVE_PASSWORD" "$TEST_ROOT/test_data"

echo -e "\n${GREEN}=== Testing GET (Download and Verify) ===${NC}"
RESTORE_DIR="$TEST_ROOT/restore"
mkdir -p "$RESTORE_DIR"
cp "test_data.mbox" "$RESTORE_DIR/"
cd "$RESTORE_DIR"

"$MBOX_BIN" get -p "$MBOX_ARCHIVE_PASSWORD" "test_data.mbox"

md5sum "test_data/file1.bin" > "$TEST_ROOT/new1.md5"
md5sum "test_data/file2.bin" > "$TEST_ROOT/new2.md5"

if diff "$TEST_ROOT/orig1.md5" "$TEST_ROOT/new1.md5" && diff "$TEST_ROOT/orig2.md5" "$TEST_ROOT/new2.md5"; then
    echo -e "${GREEN}SUCCESS: Files restored perfectly.${NC}"
else
    echo -e "${RED}FAILURE: Checksum mismatch!${NC}"
    exit 1
fi

echo -e "\n${GREEN}=== Testing Resume Logic (Simulated Failure) ===${NC}"
cd "$TEST_ROOT"
rm -rf ~/.mbox/state
rm -f resume_test.mbox

# Patch to fail after 10MB
perl -i -pe 's/wErr := c\.vfs\.Write/if offset >= 10 * 1024 * 1024 { panic("simulated failure") }\n\t\t\t\twErr := c\.vfs\.Write/' pkg/mbox/client.go
go build -o mbox ./cmd/mbox
MBOX_BIN=$(realpath ./mbox)

echo "First attempt (should fail)"
"$MBOX_BIN" stash -p "$MBOX_ARCHIVE_PASSWORD" "$TEST_ROOT/test_data" || echo "failed as expected"

echo "Restoring code and resuming"
git checkout pkg/mbox/client.go
go build -o mbox ./cmd/mbox
MBOX_BIN=$(realpath ./mbox)

"$MBOX_BIN" stash -p "$MBOX_ARCHIVE_PASSWORD" "$TEST_ROOT/test_data"

echo "Verifying resumed upload"
mkdir -p "$TEST_ROOT/restore_resume"
cp "test_data.mbox" "$TEST_ROOT/restore_resume/"
cd "$TEST_ROOT/restore_resume"
"$MBOX_BIN" get -p "$MBOX_ARCHIVE_PASSWORD" "test_data.mbox"

md5sum "test_data/file1.bin" > "$TEST_ROOT/resume1.md5"
if diff "$TEST_ROOT/orig1.md5" "$TEST_ROOT/resume1.md5"; then
    echo -e "${GREEN}SUCCESS: Resumed upload verified.${NC}"
else
    echo -e "${RED}FAILURE: Resume verification failed.${NC}"
    exit 1
fi

echo -e "\n${GREEN}CI Regression Tests Completed Successfully!${NC}"
