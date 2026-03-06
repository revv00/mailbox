#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

# Portability Helpers
get_md5() {
    if command -v md5sum >/dev/null 2>&1; then
        md5sum "$1" | awk '{print $1}'
    else
        md5 -q "$1"
    fi
}

get_size() {
    if stat --version 2>/dev/null | grep -q GNU; then
        stat -c%s "$1"
    else
        stat -f%z "$1"
    fi
}

echo -e "${GREEN}Building mbox CLI...${NC}"
go build -o mbox ./cmd/mbox

# Setup test environment
TEST_ROOT=$(mktemp -d)
trap 'rm -rf "$TEST_ROOT"' EXIT

echo "Test Workspace: $TEST_ROOT"

# Fake HOME to isolate configuration
export HOME="$TEST_ROOT"

# Locate a valid configuration file
CONFIG_SRC="mailfs-accounts.json"
if [ ! -f "$CONFIG_SRC" ]; then
    CONFIG_SRC="pkg/object/mailfs_conf_multi_gmail.json"
fi

if [ ! -f "$CONFIG_SRC" ]; then
    echo -e "${RED}Error: No test configuration found. Please create 'mailfs-accounts.json' with valid accounts.${NC}"
    exit 1
fi

echo "Using account config: $CONFIG_SRC"
mkdir -p "$HOME/.mbox"
if [ -f "$HOME/.mbox/accounts.json" ]; then
    mv "$HOME/.mbox/accounts.json" "$HOME/.mbox/accounts.json.bak"
fi
cp "$CONFIG_SRC" "$HOME/.mbox/accounts.json"

# Create a test file
TEST_FILENAME="test_document.txt"
TEST_CONTENT="Hello MailFS World - $(date)"
echo "$TEST_CONTENT" > "$TEST_FILENAME"
ORIGINAL_SUM=$(get_md5 "$TEST_FILENAME")
FILE_SIZE=$(get_size "$TEST_FILENAME")

echo -e "\n${GREEN}=== Testing STASH (Upload) ===${NC}"
# Use absolute path for mbox binary
MBOX_BIN=$(realpath ./mbox)
PASSWORD="test_password"

# Stash the file
"$MBOX_BIN" stash -p "$PASSWORD" "$TEST_FILENAME"

STICK_FILE="${TEST_FILENAME}.mbox"
if [ -f "$STICK_FILE" ]; then
    echo "Stick file created: $STICK_FILE"
else
    echo -e "${RED}Error: Stick file was not created.${NC}"
    exit 1
fi

echo -e "\n${GREEN}=== Testing GET (Download) ===${NC}"
# Create a separate restore directory
RESTORE_DIR="$TEST_ROOT/restore"
mkdir -p "$RESTORE_DIR"

# Move stick file to restore dir to simulate extraction in clean env
cp "$STICK_FILE" "$RESTORE_DIR/"
cd "$RESTORE_DIR"

"$MBOX_BIN" get -p "$PASSWORD" "$STICK_FILE"

if [ ! -f "$TEST_FILENAME" ]; then
    echo -e "${RED}Error: File was not restored.${NC}"
    ls -la
    exit 1
fi

RESTORED_SUM=$(get_md5 "$TEST_FILENAME")

if [ "$ORIGINAL_SUM" == "$RESTORED_SUM" ]; then
    echo -e "${GREEN}SUCCESS: File restored perfectly.${NC}"
    echo "Original Checksum: $ORIGINAL_SUM"
    echo "Restored Checksum: $RESTORED_SUM"
else
    echo -e "${RED}FAILURE: Checksum mismatch!${NC}"
    echo "Original: $ORIGINAL_SUM"
    echo "Restored: $RESTORED_SUM"
    exit 1
fi

# Test Case 2: Filename with spaces
echo -e "\n${GREEN}=== Testing File with Spaces ===${NC}"
cd "$TEST_ROOT"
TEST_FILENAME_SPACES="test file with spaces.txt"
TEST_CONTENT_SPACES="Hello from the file with spaces - $(date)"
echo "$TEST_CONTENT_SPACES" > "$TEST_FILENAME_SPACES"
ORIGINAL_SUM_SPACES=$(get_md5 "$TEST_FILENAME_SPACES")

"$MBOX_BIN" stash -p "$PASSWORD" "$TEST_FILENAME_SPACES"

echo -e "\n${GREEN}=== Testing LIST (mbox ls) ===${NC}"
"$MBOX_BIN" ls | grep "$TEST_FILENAME_SPACES"
if [ $? -ne 0 ]; then
    echo -e "${RED}Error: Full filename with spaces not found in 'mbox ls' output.${NC}"
    "$MBOX_BIN" ls
    exit 1
fi
echo -e "${GREEN}SUCCESS: Full filename with spaces found in 'mbox ls'.${NC}"

STICK_FILE_SPACES="${TEST_FILENAME_SPACES}.mbox"
if [ ! -f "$STICK_FILE_SPACES" ]; then
    echo -e "${RED}Error: Stick file with spaces was not created.${NC}"
    exit 1
fi

# Restore test
RESTORE_DIR_SPACES="$TEST_ROOT/restore_spaces"
mkdir -p "$RESTORE_DIR_SPACES"
cp "$STICK_FILE_SPACES" "$RESTORE_DIR_SPACES/"
cd "$RESTORE_DIR_SPACES"

"$MBOX_BIN" get -p "$PASSWORD" "$STICK_FILE_SPACES"

if [ ! -f "$TEST_FILENAME_SPACES" ]; then
    echo -e "${RED}Error: File with spaces was not restored.${NC}"
    ls -la
    exit 1
fi

RESTORED_SUM_SPACES=$(get_md5 "$TEST_FILENAME_SPACES")
if [ "$ORIGINAL_SUM_SPACES" != "$RESTORED_SUM_SPACES" ]; then
    echo -e "${RED}FAILURE: Checksum mismatch for file with spaces!${NC}"
    echo "Original: $ORIGINAL_SUM_SPACES"
    echo "Restored: $RESTORED_SUM_SPACES"
    exit 1
fi
echo -e "${GREEN}SUCCESS: File with spaces restored perfectly.${NC}"

echo -e "\n${GREEN}Test Sequence Completed Successfully!${NC}"
if [ -f "$HOME/.mbox/accounts.json.bak" ]; then
    mv "$HOME/.mbox/accounts.json.bak" "$HOME/.mbox/accounts.json"
fi
