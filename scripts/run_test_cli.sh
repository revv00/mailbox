#!/bin/bash
set -e

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

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
ORIGINAL_SUM=$(md5sum "$TEST_FILENAME" | awk '{print $1}')
FILE_SIZE=$(stat -c%s "$TEST_FILENAME")

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

RESTORED_SUM=$(md5sum "$TEST_FILENAME" | awk '{print $1}')

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

echo -e "\n${GREEN}Test Sequence Completed Successfully!${NC}"
if [ -f "$HOME/.mbox/accounts.json.bak" ]; then
    mv "$HOME/.mbox/accounts.json.bak" "$HOME/.mbox/accounts.json"
fi
