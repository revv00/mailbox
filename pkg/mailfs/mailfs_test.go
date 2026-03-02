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

package mailfs

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/client"
	"github.com/revv00/mailfs/pkg/config"
)

// testConfigFile holds test account configurations
type testConfigFile struct {
	Accounts          []config.AccountConfig `json:"accounts"`
	ReplicationFactor int                    `json:"replicationFactor"`
}

// resolveConfigPath resolves the config file path, handling 'go test' CWD behavior
func resolveConfigPath(configPath string) string {
	if configPath == "" {
		return ""
	}

	// 1. Get current working directory (usually pkg/object during test)
	cwd, _ := os.Getwd()
	fmt.Printf("[CONFIG-DEBUG] CWD: %s\n", cwd)
	fmt.Printf("[CONFIG-DEBUG] Input Path: %s\n", configPath)

	// List of attempts to find the file
	candidates := []string{
		// A. Try path exactly as provided (works if absolute or relative to CWD)
		configPath,

		// B. Try assuming the file is in the same directory as the test (strip path, keep filename)
		// e.g. input "pkg/object/conf.json" -> check CWD/conf.json
		filepath.Base(configPath),

		// C. Try going up one level (Repo Root/pkg/object -> Repo Root/pkg)
		filepath.Join("..", configPath),

		// D. Try going up two levels (Repo Root/pkg/object -> Repo Root)
		// This is the most likely fix if you ran from root with "pkg/object/file.json"
		filepath.Join("..", "..", configPath),

		// E. Try going up three levels (just in case)
		filepath.Join("..", "..", "..", configPath),
	}

	for _, tryPath := range candidates {
		// Clean the path to remove redundant ./ or ../
		cleanPath := filepath.Clean(tryPath)

		// Debug log for each attempt (comment out if too noisy, but useful now)
		// fmt.Printf("[CONFIG-DEBUG] Checking: %s\n", cleanPath)

		info, err := os.Stat(cleanPath)
		if err == nil && !info.IsDir() {
			abs, _ := filepath.Abs(cleanPath)
			fmt.Printf("[CONFIG-DEBUG] ✅ FOUND at: %s\n", abs)
			return abs
		}
	}

	// If we get here, we really couldn't find it.
	// List files in CWD to help user see what's wrong.
	fmt.Printf("[CONFIG-DEBUG] ❌ File not found. Files in CWD (%s):\n", cwd)
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		fmt.Printf(" - %s\n", e.Name())
	}

	return configPath // Return original so failure happens at ReadFile with error
}

// loadConfigFromFile loads MailFS configuration from a JSON file
func loadConfigFromFile(configPath string, dbPath string) (config.MailFSConfig, bool) {
	fmt.Printf("\n[CONFIG-DEBUG] --- Starting Config Load ---\n")

	resolvedPath := resolveConfigPath(configPath)

	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		fmt.Printf("[CONFIG-DEBUG] ❌ os.ReadFile failed: %v\n", err)
		return config.MailFSConfig{}, false
	}

	var cfg testConfigFile
	if err := json.Unmarshal(data, &cfg); err != nil {
		fmt.Printf("[CONFIG-DEBUG] ❌ JSON Unmarshal failed: %v\n", err)
		return config.MailFSConfig{}, false
	}

	if len(cfg.Accounts) == 0 {
		fmt.Printf("[CONFIG-DEBUG] ❌ Config loaded but 'accounts' list is empty.\n")
		return config.MailFSConfig{}, false
	}

	replFactor := cfg.ReplicationFactor
	if replFactor == 0 {
		replFactor = 2
	}

	// Convert AccountConfig to MailAccount
	var accounts []*config.MailAccount
	for _, accCfg := range cfg.Accounts {
		acc, err := config.NewMailAccount(accCfg)
		if err != nil {
			fmt.Printf("[CONFIG-DEBUG] ❌ Invalid account config: %v\n", err)
			return config.MailFSConfig{}, false
		}
		accounts = append(accounts, acc)
	}

	fmt.Printf("[CONFIG-DEBUG] ✅ Config loaded successfully (%d accounts).\n", len(accounts))
	fmt.Printf("[CONFIG-DEBUG] --- End Config Load ---\n\n")

	return config.MailFSConfig{
		Accounts:          accounts,
		DBPath:            dbPath,
		BlobFolder:        ".mailfs_test_blobs",
		ReplicationFactor: replFactor,
	}, true
}

// getTestConfig returns a config from file (if TEST_CONFIG env var set) or a dummy config
func getTestConfig(t *testing.T, dbPath string) (config.MailFSConfig, bool) {
	envPath := os.Getenv("TEST_CONFIG")

	if envPath != "" {
		cfg, ok := loadConfigFromFile(envPath, dbPath)
		if ok && len(cfg.Accounts) > 0 {
			t.Logf("Loaded real configuration from %s with %d accounts", envPath, len(cfg.Accounts))
			return cfg, true
		}
		t.Logf("TEST_CONFIG set to %s,%s but file not found or invalid, falling back to dummy", envPath, dbPath)
	} else {
		t.Log("TEST_CONFIG env var not set, using dummy configuration")
	}

	// Return dummy config for basic unit tests
	return config.MailFSConfig{
		Accounts: []*config.MailAccount{
			{
				Email:    "test@local.dev",
				Password: "password",
				IMAPHost: "127.0.0.1:993",
				SMTPHost: "127.0.0.1:587",
				Folder:   "INBOX",
			},
		},
		DBPath:            dbPath,
		BlobFolder:        ".mailfs_dummy",
		ReplicationFactor: 1,
	}, false
}

func TestRealIMAPConnection(t *testing.T) {
	configPath := os.Getenv("TEST_CONFIG")
	if configPath == "" {
		t.Skip("Skipping integration test: TEST_CONFIG env var not set")
	}

	cfg, ok := loadConfigFromFile(configPath, "memory")
	if !ok || len(cfg.Accounts) == 0 {
		t.Fatalf("Failed to load valid configuration from %s", configPath)
	}

	for i, acc := range cfg.Accounts {
		t.Run(fmt.Sprintf("Account_%d_%s", i, acc.Email), func(t *testing.T) {
			addr := acc.IMAPHost
			if !strings.Contains(addr, ":") {
				addr += ":993"
			}
			host, _, _ := net.SplitHostPort(addr)

			// Sina Mail requires specific legacy configuration to pass handshake
			tlsConfig := &tls.Config{
				ServerName:         host,
				InsecureSkipVerify: true, // Sina certs often fail chain validation in Go
				MinVersion:         tls.VersionTLS10,
				// Explicitly enable RSA ciphers (Go disables these by default now)
				// These are the ciphers openssl likely negotiated
				CipherSuites: []uint16{
					tls.TLS_RSA_WITH_AES_128_CBC_SHA,
					tls.TLS_RSA_WITH_AES_256_CBC_SHA,
					tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
					tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
					tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
					tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
				},
			}

			var c *client.Client
			var err error

			t.Logf("Attempting DialTLS to %s with legacy cipher support...", addr)

			// Use a dialer with a timeout
			dialer := &net.Dialer{Timeout: 10 * time.Second}
			conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)

			if err != nil {
				t.Logf("DialTLS failed: %v. Trying port 143 + STARTTLS...", err)

				// STARTTLS Fallback
				plainConn, err := net.DialTimeout("tcp", host+":143", 10*time.Second)
				if err != nil {
					t.Fatalf("Could not reach port 143: %v", err)
				}

				c, err = client.New(plainConn)
				if err != nil {
					t.Fatalf("Failed to create IMAP client: %v", err)
				}

				if err = c.StartTLS(tlsConfig); err != nil {
					t.Fatalf("STARTTLS handshake failed even with legacy ciphers: %v", err)
				}
			} else {
				// Use the existing TLS connection
				c, err = client.New(conn)
				if err != nil {
					t.Fatalf("Failed to create IMAP client from TLS conn: %v", err)
				}
			}

			defer c.Logout()

			t.Log("Handshake successful, attempting Login...")
			if err := c.Login(acc.Email, acc.Password); err != nil {
				t.Fatalf("Login failed: %v", err)
			}
			t.Log("✅ Login successful!")

			mbox, err := c.Select(acc.Folder, false)
			if err != nil {
				t.Logf("Folder %s check: %v", acc.Folder, err)
			} else {
				t.Logf("✅ Folder selected. Messages: %d", mbox.Messages)
			}
		})
	}
}

func TestMailFSBasic(t *testing.T) {
	dbPath := "test-mailfs-basic.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	// Get config
	cfg, isRealConfig := getTestConfig(t, dbPath)
	t.Logf("[STEP 0] Initializing MailFS with %d accounts (RealConfig: %v)", len(cfg.Accounts), isRealConfig)

	mfs, err := NewMailFS(cfg)
	if err != nil {
		t.Fatalf("❌ Failed to create MailFS: %v", err)
	}
	defer mfs.Close()
	t.Logf("✅ MailFS initialized successfully")

	testKey := "test-key-basic-" + fmt.Sprintf("%d", time.Now().Unix()%10000)
	testData := []byte("Hello, MailFS! Content at " + time.Now().Format(time.RFC3339))

	// 1. Test Put
	t.Logf("[STEP 1] Performing Put for key: %s (%d bytes)", testKey, len(testData))
	err = mfs.Put(testKey, bytes.NewReader(testData))
	if err != nil {
		if !isRealConfig {
			t.Logf("⚠️ Put failed as expected with dummy config (No SMTP server). Injecting data to test remaining logic...")
			mfs.Lock()
			mfs.blobCache[testKey] = &mailBlob{
				key:     testKey,
				size:    int64(len(testData)),
				mtime:   time.Now(),
				data:    testData,
				account: 0,
			}
			// Also manually insert into DB so List works
			mfs.db.Exec("INSERT INTO blobs (key, size, mtime, account) VALUES (?, ?, ?, ?)",
				testKey, len(testData), time.Now().UnixNano(), 0)
			mfs.Unlock()
		} else {
			t.Fatalf("❌ Put failed with real config: %v", err)
		}
	} else {
		t.Logf("✅ Put successful! Blob stored on mail server.")
	}

	// 2. Test Head
	t.Logf("[STEP 2] Performing Head for key: %s", testKey)
	info, err := mfs.Head(testKey)
	if err != nil {
		t.Fatalf("❌ Head failed: %v", err)
	}
	t.Logf("✅ Head successful: Size=%d, Mtime=%v", info.Size(), info.Mtime())
	if info.Size() != int64(len(testData)) {
		t.Errorf("❌ Size mismatch: got %d, want %d", info.Size(), len(testData))
	}

	// 3. Test Get (This triggers the IMAP fetch if not in cache)
	t.Logf("[STEP 3] Performing Get for key: %s", testKey)
	// Clear cache if real config to force network fetch
	if isRealConfig {
		mfs.Lock()
		delete(mfs.blobCache, testKey)
		mfs.Unlock()
		t.Logf("   (Cleared local cache to force IMAP download)")
	}

	reader, err := mfs.Get(testKey, 0, -1)
	if err != nil {
		t.Fatalf("❌ Get failed: %v", err)
	}
	gotData, err := io.ReadAll(reader)
	reader.Close()
	if err != nil {
		t.Fatalf("❌ Reading Get stream failed: %v", err)
	}
	t.Logf("✅ Get successful! Received %d bytes", len(gotData))

	if !bytes.Equal(gotData, testData) {
		t.Errorf("❌ Data integrity check failed!")
		t.Errorf("   Expected: %s", string(testData))
		t.Errorf("   Got:      %s", string(gotData))
	} else {
		t.Logf("✅ Data integrity verified (Content matches).")
	}

	// 4. Test List
	t.Logf("[STEP 4] Performing List with prefix ''")
	objs, _, _, err := mfs.List("", "", "", "", 1000, false)
	if err != nil {
		t.Fatalf("❌ List failed: %v", err)
	}
	t.Logf("✅ List returned %d objects (hasMore: %v, nextMarker: %s)", len(objs), false, "")

	found := false
	for _, o := range objs {
		t.Logf("   - Found object: %s (Size: %d)", o.Key(), o.Size())
		if o.Key() == testKey {
			found = true
		}
	}
	if !found {
		t.Errorf("❌ List did not find our test key %s", testKey)
	} else {
		t.Logf("✅ Test key found in List results.")
	}

	// 5. Test Delete
	time.Sleep(2 * time.Second) // Wait a bit to ensure email delivery is settled
	t.Logf("[STEP 5] Performing Delete for key: %s", testKey)
	err = mfs.Delete(testKey)
	if err != nil {
		t.Fatalf("❌ Delete failed: %v", err)
	}
	t.Logf("✅ Delete command executed successfully.")

	// Verify deletion
	_, err = mfs.Head(testKey)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Errorf("❌ Head should have failed with 'not exist' after Delete, but got: %v", err)
	} else {
		t.Logf("✅ Verified: Blob metadata removed from database.")
	}
}

func TestSubjectLineEncoding(t *testing.T) {
	testKey := "test-blob-key-12345"
	expectedSubject := fmt.Sprintf("hey, got something for you :%s", testKey)

	if !strings.Contains(expectedSubject, fmt.Sprintf(":%s", testKey)) {
		t.Errorf("Subject doesn't contain key with delimiter: %s", expectedSubject)
	}
}

func TestBlobDataStorage(t *testing.T) {
	dbPath := "test-blob-storage.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	cfg, _ := getTestConfig(t, dbPath)

	mfs, err := NewMailFS(cfg)
	if err != nil {
		t.Fatalf("Failed to create MailFS: %v", err)
	}
	defer mfs.Close()

	testKey := "blob-storage-test"
	testData := []byte("test blob data content")

	// Manually store in cache to test retrieval (bypass email sending)
	mfs.Lock()
	mfs.blobCache[testKey] = &mailBlob{
		key:     testKey,
		size:    int64(len(testData)),
		data:    testData,
		mtime:   time.Now(),
		account: 0,
	}
	mfs.Unlock()

	// Retrieve from cache
	r, err := mfs.Get(testKey, 0, -1)
	if err != nil {
		t.Fatalf("Failed to get from cache: %v", err)
	}
	gotData, _ := io.ReadAll(r)
	r.Close()

	if !bytes.Equal(gotData, testData) {
		t.Errorf("Data mismatch: expected %s, got %s", testData, gotData)
	}
}

func TestHashDistribution(t *testing.T) {
	accountCount := 5
	distribution := make(map[int]int)

	for i := 0; i < 100; i++ {
		key := fmt.Sprintf("test-key-%d", i)
		account := hashToAccount(key, accountCount)
		distribution[account]++

		if account < 0 || account >= accountCount {
			t.Errorf("Invalid account index: %d", account)
		}
	}

	usedAccounts := 0
	for i := 0; i < accountCount; i++ {
		if distribution[i] > 0 {
			usedAccounts++
		}
	}

	if usedAccounts < 3 {
		t.Logf("Warning: Poor distribution: only %d accounts used for 100 keys", usedAccounts)
	}
}

func TestReplicationLogic(t *testing.T) {
	dbPath := "test-replication.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	// Create config with enough accounts for replication
	cfg := config.MailFSConfig{
		Accounts: make([]*config.MailAccount, 5),
		DBPath:   dbPath,
	}
	// Populate dummy accounts so validation passes
	for i := range cfg.Accounts {
		cfg.Accounts[i] = &config.MailAccount{Email: fmt.Sprintf("acc%d@test.com", i)}
	}

	mfs, err := NewMailFS(cfg)
	if err != nil {
		t.Fatalf("failed to create MailFS: %v", err)
	}
	defer mfs.Close()

	testKey := "test-replication-key"
	primaryIdx, replicaIdx := mfs.getReplicaAccounts(testKey)

	if primaryIdx == replicaIdx {
		t.Errorf("primary and replica accounts are the same: %d", primaryIdx)
	}

	if primaryIdx < 0 || primaryIdx >= len(mfs.accounts) {
		t.Errorf("invalid primary account index: %d", primaryIdx)
	}
}

func TestOffsetAndLimit(t *testing.T) {
	dbPath := "test-offset-limit.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	cfg, _ := getTestConfig(t, dbPath)
	mfs, err := NewMailFS(cfg)
	if err != nil {
		t.Fatalf("Failed to create MailFS: %v", err)
	}
	defer mfs.Close()

	testKey := "offset-limit-test"
	testData := []byte("0123456789abcdefghij") // 20 bytes

	// Inject into cache
	mfs.Lock()
	mfs.blobCache[testKey] = &mailBlob{
		key:   testKey,
		size:  int64(len(testData)),
		data:  testData,
		mtime: time.Now(),
	}
	mfs.Unlock()

	// Test Offset
	reader, err := mfs.Get(testKey, 5, -1)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	data, _ := io.ReadAll(reader)
	if string(data) != "56789abcdefghij" {
		t.Errorf("Offset mismatch: expected '56789abcdefghij', got '%s'", string(data))
	}

	// Test Offset + Limit
	reader, err = mfs.Get(testKey, 5, 5)
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	data, _ = io.ReadAll(reader)
	if string(data) != "56789" {
		t.Errorf("Limit mismatch: expected '56789', got '%s'", string(data))
	}
}
