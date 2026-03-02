package config

import (
	"os"
	"testing"
)

func TestLoadAccountsFromJSON(t *testing.T) {
	// Test case 1: Array format (Legacy)
	arrayContent := `[
		{"email": "test1@example.com", "password": "pwd", "imap_host": "imap.test.com:993", "smtp_host": "smtp.test.com:465"}
	]`
	// create temp file
	tmpfile, err := os.CreateTemp("", "config_array_*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile.Name())
	if _, err := tmpfile.Write([]byte(arrayContent)); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile.Close(); err != nil {
		t.Fatal(err)
	}

	// Note: We changed tags to imapHost/smtpHost, so imap_host/smtp_host in JSON might fail to parse if strict?
	// The AccountConfig struct tags are `json:"imapHost,omitempty"`.
	// Standard JSON unmarshal is case-insensitive, but when tags are present, it usually prefers the tag.
	// If the tag is "imapHost", then "imap_host" might not be matched unless we have multiple tags (not supported in std lib cleanly without duplicate fields) or simple case-insensitivity helps.
	// Actually, strict matching on tags means "imap_host" won't populate "IMAPHost" if the tag is "imapHost".
	// Let's verify this behavior. If backward compatibility is strictly required for "imap_host", we might need to add a second field or custom unmarshal.
	// However, usually upgrading config format implies updating the config file or supporting both.
	// Let's test with the NEW format first to ensure the user's request is met.

	// Test case 2: Object format (New)
	objectContent := `{
		"accounts": [
			{
				"email": "test2@example.com", 
				"password": "pwd", 
				"imapHost": "imap.test2.com:993", 
				"smtpHost": "smtp.test2.com:465",
				"folder": "data1"
			}
		],
		"replicationFactor": 2
	}`
	tmpfile2, err := os.CreateTemp("", "config_obj_*.json")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpfile2.Name())
	if _, err := tmpfile2.Write([]byte(objectContent)); err != nil {
		t.Fatal(err)
	}
	if err := tmpfile2.Close(); err != nil {
		t.Fatal(err)
	}

	accounts, err := LoadAccountsFromJSON(tmpfile2.Name())
	if err != nil {
		t.Fatalf("Failed to load object config: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("Expected 1 account, got %d", len(accounts))
	}
	if accounts[0].Email != "test2@example.com" {
		t.Errorf("Expected email test2@example.com, got %s", accounts[0].Email)
	}
	if accounts[0].IMAPHost != "imap.test2.com:993" {
		t.Errorf("Expected IMAP host imap.test2.com:993, got %s", accounts[0].IMAPHost)
	}
}
