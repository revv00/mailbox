/*
 * JuiceFS, Copyright 2020 Juicedata, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/revv00/mailfs/pkg/crypto"
)

// MailAccount represents email configuration for storing blobs
type MailAccount struct {
	Email    string
	Password string
	IMAPHost string
	SMTPHost string
	Folder   string // IMAP folder for storing blobs
	mu       sync.Mutex
}

func (m *MailAccount) Lock() {
	m.mu.Lock()
}

func (m *MailAccount) Unlock() {
	m.mu.Unlock()
}

// MailFSConfig holds configuration for mailfs
type MailFSConfig struct {
	Accounts          []*MailAccount
	DBPath            string // SQLite database path for metadata
	BlobFolder        string // Folder prefix for blobs in email
	ReplicationFactor int    // Number of replicas per chunk
	NoCache           bool   // If true, data is not stored in local DB
}

// MailProvider represents common email providers with preset configurations
type MailProvider struct {
	Name     string
	IMAPHost string
	SMTPHost string
	IMAPPort int
	SMTPPort int
}

// Common email provider configurations
var Providers = map[string]MailProvider{
	"gmail": {
		Name:     "Gmail",
		IMAPHost: "imap.gmail.com:993",
		SMTPHost: "smtp.gmail.com:587",
	},
	"outlook": {
		Name:     "Outlook",
		IMAPHost: "outlook.office365.com:993",
		SMTPHost: "smtp.office365.com:587",
	},
	"yahoo": {
		Name:     "Yahoo",
		IMAPHost: "imap.mail.yahoo.com:993",
		SMTPHost: "smtp.mail.yahoo.com:587",
	},
	"protonmail": {
		Name:     "ProtonMail",
		IMAPHost: "imap.protonmail.com:993",
		SMTPHost: "smtp.protonmail.com:587",
	},
	"icloud": {
		Name:     "iCloud",
		IMAPHost: "imap.mail.me.com:993",
		SMTPHost: "smtp.mail.me.com:587",
	},
	"163": {
		Name:     "NetEase 163",
		IMAPHost: "imap.163.com:993",
		SMTPHost: "smtp.163.com:465",
	},
	"qq": {
		Name:     "QQ Mail",
		IMAPHost: "imap.qq.com:993",
		SMTPHost: "smtp.qq.com:587",
	},
	"sina": {
		Name:     "Sina Mail",
		IMAPHost: "imap.sina.com:993",
		SMTPHost: "smtp.sina.com:465",
	},
}

// AccountConfig is used for loading accounts from JSON/YAML
type AccountConfig struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Provider string `json:"provider,omitempty"` // gmail, outlook, yahoo, etc.
	IMAPHost string `json:"imapHost,omitempty"` // camelCase for JSON compatibility
	SMTPHost string `json:"smtpHost,omitempty"` // camelCase for JSON compatibility
	Folder   string `json:"folder,omitempty"`
}

// ParsedConfig holds accounts and global settings
type ParsedConfig struct {
	Accounts    []*MailAccount
	Replication int
}

// LoadAccountsFromJSON loads email accounts and global settings from JSON file
func LoadAccountsFromJSON(filePath string) (*ParsedConfig, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var configs []AccountConfig
	var replication int

	// Try parsing as object with "accounts" key
	var wrapper struct {
		Accounts    []AccountConfig `json:"accounts"`
		Replication int             `json:"replication"`
	}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Accounts) > 0 {
		configs = wrapper.Accounts
		replication = wrapper.Replication
	} else {
		// Fallback to array of configs (old format)
		if err := json.Unmarshal(data, &configs); err != nil {
			return nil, fmt.Errorf("failed to parse JSON: %w", err)
		}
	}

	var accounts []*MailAccount
	for _, cfg := range configs {
		acc, err := NewMailAccount(cfg)
		if err != nil {
			return nil, fmt.Errorf("invalid account config for %s: %w", cfg.Email, err)
		}
		accounts = append(accounts, acc)
	}

	return &ParsedConfig{Accounts: accounts, Replication: replication}, nil
}

// LoadAccountsFromEncryptedJSON loads email accounts and global settings from an encrypted JSON file
func LoadAccountsFromEncryptedJSON(filePath string, password string) (*ParsedConfig, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Try decrypting
	decrypted, err := crypto.Decrypt(password, data)
	if err != nil {
		return nil, fmt.Errorf("failed to decrypt config: %w", err)
	}

	var configs []AccountConfig
	var replication int
	var wrapper struct {
		Accounts    []AccountConfig `json:"accounts"`
		Replication int             `json:"replication"`
	}
	if err := json.Unmarshal(decrypted, &wrapper); err == nil && len(wrapper.Accounts) > 0 {
		configs = wrapper.Accounts
		replication = wrapper.Replication
	} else {
		if err := json.Unmarshal(decrypted, &configs); err != nil {
			return nil, fmt.Errorf("failed to parse decrypted JSON: %w", err)
		}
	}

	var accounts []*MailAccount
	for _, cfg := range configs {
		acc, err := NewMailAccount(cfg)
		if err != nil {
			return nil, fmt.Errorf("invalid account config for %s: %w", cfg.Email, err)
		}
		accounts = append(accounts, acc)
	}

	return &ParsedConfig{Accounts: accounts, Replication: replication}, nil
}

// SaveAccountsEncrypted saves accounts to a file encrypted with a password
func SaveAccountsEncrypted(filePath string, accounts []*MailAccount, replication int, password string) error {
	configs := make([]AccountConfig, len(accounts))
	for i, acc := range accounts {
		configs[i] = AccountConfig{
			Email:    acc.Email,
			Password: acc.Password,
			IMAPHost: acc.IMAPHost,
			SMTPHost: acc.SMTPHost,
			Folder:   acc.Folder,
		}
	}

	wrapper := struct {
		Accounts    []AccountConfig `json:"accounts"`
		Replication int             `json:"replication,omitempty"`
	}{
		Accounts:    configs,
		Replication: replication,
	}

	data, err := json.MarshalIndent(wrapper, "", "  ")
	if err != nil {
		return err
	}

	encrypted, err := crypto.Encrypt(password, data)
	if err != nil {
		return err
	}

	return os.WriteFile(filePath, encrypted, 0600)
}

// SerializeAccounts serializes accounts to JSON bytes (for portable archives)
func SerializeAccounts(accounts []*MailAccount, replication int) ([]byte, error) {
	configs := make([]AccountConfig, len(accounts))
	for i, acc := range accounts {
		configs[i] = AccountConfig{
			Email:    acc.Email,
			Password: acc.Password,
			IMAPHost: acc.IMAPHost,
			SMTPHost: acc.SMTPHost,
			Folder:   acc.Folder,
		}
	}
	wrapper := struct {
		Accounts    []AccountConfig `json:"accounts"`
		Replication int             `json:"replication,omitempty"`
	}{
		Accounts:    configs,
		Replication: replication,
	}
	return json.MarshalIndent(wrapper, "", "  ")
}

// IsEncrypted checks if the file at filePath is likely encrypted
func IsEncrypted(filePath string) (bool, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return false, err
	}
	if len(data) < 1 {
		return false, nil
	}
	// JSON should start with { or [
	return data[0] != '{' && data[0] != '[', nil
}

// NewMailAccount creates a MailAccount from AccountConfig
func NewMailAccount(cfg AccountConfig) (*MailAccount, error) {
	if cfg.Email == "" {
		return nil, fmt.Errorf("email is required")
	}
	if cfg.Password == "" {
		return nil, fmt.Errorf("password is required")
	}

	acc := &MailAccount{
		Email:    cfg.Email,
		Password: cfg.Password,
		Folder:   "juicefs-blobs",
	}

	if cfg.Folder != "" {
		acc.Folder = cfg.Folder
	}

	// Use provider preset if specified
	if cfg.Provider != "" {
		provider, ok := Providers[strings.ToLower(cfg.Provider)]
		if !ok {
			return nil, fmt.Errorf("unknown provider: %s", cfg.Provider)
		}
		acc.IMAPHost = provider.IMAPHost
		acc.SMTPHost = provider.SMTPHost
	}

	// Override with explicit hosts if provided
	if cfg.IMAPHost != "" {
		acc.IMAPHost = cfg.IMAPHost
	}
	if cfg.SMTPHost != "" {
		acc.SMTPHost = cfg.SMTPHost
	}

	// Validate
	if acc.IMAPHost == "" {
		return nil, fmt.Errorf("IMAP host is required (specify provider or imap_host)")
	}
	if acc.SMTPHost == "" {
		return nil, fmt.Errorf("SMTP host is required (specify provider or smtp_host)")
	}

	return acc, nil
}

// GenerateConfigTemplate generates a sample configuration file
func GenerateConfigTemplate(w io.Writer, numAccounts int) error {
	if numAccounts <= 0 {
		numAccounts = 5
	}

	configs := make([]AccountConfig, numAccounts)
	providers := []string{"gmail", "outlook", "yahoo", "protonmail", "icloud"}

	for i := 0; i < numAccounts && i < len(providers); i++ {
		configs[i] = AccountConfig{
			Email:    fmt.Sprintf("your-account-%d@%s.com", i+1, strings.TrimSuffix(providers[i], "mail")),
			Password: "your-app-password",
			Provider: providers[i],
			Folder:   "juicefs-blobs",
		}
	}

	data, err := json.MarshalIndent(configs, "", "  ")
	if err != nil {
		return err
	}

	_, err = w.Write(data)
	return err
}

// ValidateAccounts checks if accounts are properly configured
func ValidateAccounts(accounts []*MailAccount) error {
	if len(accounts) == 0 {
		return fmt.Errorf("at least one email account is required")
	}
	if len(accounts) > 10 {
		return fmt.Errorf("maximum 10 accounts supported (got %d)", len(accounts))
	}

	emailSet := make(map[string]bool)
	for i, acc := range accounts {
		if acc.Email == "" {
			return fmt.Errorf("account %d: email is empty", i)
		}
		if emailSet[acc.Email] {
			return fmt.Errorf("duplicate email: %s", acc.Email)
		}
		emailSet[acc.Email] = true

		if acc.IMAPHost == "" {
			return fmt.Errorf("account %d (%s): IMAP host is empty", i, acc.Email)
		}
		if acc.SMTPHost == "" {
			return fmt.Errorf("account %d (%s): SMTP host is empty", i, acc.Email)
		}
		if acc.Password == "" {
			return fmt.Errorf("account %d (%s): password is empty", i, acc.Email)
		}
	}

	return nil
}
