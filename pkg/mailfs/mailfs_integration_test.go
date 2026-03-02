package mailfs_test

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juicedata/juicefs/pkg/vfs"
	"github.com/revv00/mailfs/pkg/config"
	"github.com/revv00/mailfs/pkg/mailfs"
	"github.com/sirupsen/logrus"
)

// TestContext wraps meta.Context to satisfy vfs.Context (which requires Duration)
type TestContext struct {
	meta.Context
}

func (c *TestContext) Duration() time.Duration {
	return 0
}

// testConfigFile mimics the structure of your JSON config.
// defined locally because we are in package object_test
type testConfigFile struct {
	Accounts          []config.AccountConfig `json:"accounts"`
	ReplicationFactor int                    `json:"replication_factor"`
}

// resolveConfigPath resolves the config file path, handling 'go test' CWD behavior
func resolveConfigPath(configPath string) string {
	if configPath == "" {
		return ""
	}

	// 1. Get current working directory
	cwd, _ := os.Getwd()
	fmt.Printf("[CONFIG-DEBUG] CWD: %s\n", cwd)
	fmt.Printf("[CONFIG-DEBUG] Input Path: %s\n", configPath)

	// List of attempts to find the file
	candidates := []string{
		configPath,                                  // Exact path
		filepath.Base(configPath),                   // Same dir
		filepath.Join("..", configPath),             // Up one
		filepath.Join("..", "..", configPath),       // Up two (Repo Root)
		filepath.Join("..", "..", "..", configPath), // Up three
	}

	for _, tryPath := range candidates {
		cleanPath := filepath.Clean(tryPath)
		info, err := os.Stat(cleanPath)
		if err == nil && !info.IsDir() {
			abs, _ := filepath.Abs(cleanPath)
			fmt.Printf("[CONFIG-DEBUG] ✅ FOUND at: %s\n", abs)
			return abs
		}
	}

	// If we get here, we really couldn't find it.
	fmt.Printf("[CONFIG-DEBUG] ❌ File not found. Files in CWD (%s):\n", cwd)
	entries, _ := os.ReadDir(".")
	for _, e := range entries {
		fmt.Printf(" - %s\n", e.Name())
	}

	return configPath
}

// loadConfigFromFile loads MailFS configuration using the robust path resolution
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

func TestJuiceFSBigFileWrite(t *testing.T) {
	utils.SetLogLevel(logrus.DebugLevel)
	// 1. Setup Configuration
	configPath := os.Getenv("TEST_CONFIG")
	if configPath == "" {
		t.Skip("TEST_CONFIG not set; skipping integration test")
	}

	// Clean up DB from previous runs
	dbPath := "integration_test.db"
	os.Remove(dbPath)
	defer os.Remove(dbPath)

	cfg, ok := loadConfigFromFile(configPath, dbPath)
	if !ok {
		t.Fatalf("Failed to load config from %s", configPath)
	}

	// 2. Initialize MailFS
	blobStore, err := mailfs.NewMailFS(cfg)
	if err != nil {
		t.Fatalf("NewMailFS failed: %v", err)
	}
	var store object.ObjectStorage = blobStore

	// 3. Initialize Metadata (SQLite)
	metaPath := "test_meta.db"
	os.Remove(metaPath)
	defer os.Remove(metaPath)

	metaConf := meta.Config{Retries: 10}
	m := meta.NewClient("sqlite3://"+metaPath, &metaConf)

	format := &meta.Format{
		Name:      "testfs",
		UUID:      "test-uuid",
		Storage:   "mailfs",
		BlockSize: 4096,
		Capacity:  1 << 30,
	}
	if err := m.Init(format, true); err != nil {
		t.Fatalf("Meta Init failed: %v", err)
	}

	// 4. Initialize Chunk Store
	chunkConf := chunk.Config{
		BlockSize: 10 * 1024 * 1024,
		Compress:  "none",
		MaxUpload: 1,
	}

	// Using the constructor you provided in the prompt
	chunkStore := chunk.NewCachedStore(store, chunkConf, nil)

	// 5. Initialize VFS
	vfsConf := &vfs.Config{
		Chunk: &chunkConf,
		Meta:  &metaConf,
	}

	// vfs.NewVFS returns just the instance
	jfs := vfs.NewVFS(vfsConf, m, chunkStore, nil, nil)

	// Create the wrapper context
	baseCtx := meta.NewContext(0, 0, []uint32{0})
	ctx := &TestContext{Context: baseCtx}

	// ---------------------------------------------------------
	// THE TEST
	// ---------------------------------------------------------
	var data []byte
	fileName := "juicefs_big_test.bin"
	testFileSource := os.Getenv("TEST_FILE_TO_WRITE")

	if testFileSource != "" {
		var err error
		data, err = os.ReadFile(testFileSource)
		if err != nil {
			t.Fatalf("Failed to read TEST_FILE_TO_WRITE (%s): %v", testFileSource, err)
		}
		fileName = filepath.Base(testFileSource)
		t.Logf("Using data from file: %s (%d bytes)", testFileSource, len(data))
	} else {
		fileSize := 10 * 1024 * 1024
		t.Logf("Generating %d bytes of random data...", fileSize)
		data = make([]byte, fileSize)
		rand.Seed(time.Now().UnixNano())
		rand.Read(data)
	}
	fileSize := len(data)

	// A. CREATE
	// Signature: Create(ctx, parent, name, mode, umask, dev) -> (entry, fh, err)
	entry, fh, cErr := jfs.Create(ctx, 1, fileName, 0644, 022, uint32(os.O_RDWR))
	if cErr != 0 {
		t.Fatalf("VFS Create failed code: %v", cErr)
	}
	t.Logf("File created with Inode: %d, FH: %d", entry.Inode, fh)

	// B. WRITE
	start := time.Now()
	wErr := jfs.Write(ctx, entry.Inode, data, 0, fh)
	if wErr != 0 {
		t.Fatalf("VFS Write failed code: %v", wErr)
	}

	// C. FLUSH
	if fErr := jfs.Flush(ctx, entry.Inode, fh, 0); fErr != 0 {
		t.Fatalf("VFS Flush failed code: %v", fErr)
	}
	t.Logf("Write & Flush complete in %v", time.Since(start))

	// ---------------------------------------------------------
	// VERIFICATION
	// ---------------------------------------------------------

	// Check MailFS Internal State
	// Using generic context for object
	objs, _, _, _ := blobStore.List("", "", "", "", 100, false)
	t.Logf("Found %d chunks in MailFS:", len(objs))
	for _, o := range objs {
		t.Logf(" - Key: %s | Size: %d", o.Key(), o.Size())
	}

	if len(objs) < 1 {
		t.Errorf("Expected at least 1 chunk in storage, found %d", len(objs))
	}

	// D. READ BACK
	t.Log("Reading file back via JuiceFS...")
	readBuf := make([]byte, fileSize)

	// Signature: Read(ctx, ino, buf, offset, fh) -> (n, err)
	_, rErr := jfs.Read(ctx, entry.Inode, readBuf, 0, fh)
	if rErr != 0 {
		t.Fatalf("VFS Read failed code: %v", rErr)
	}

	if !bytesEqual(data, readBuf) {
		t.Fatal("Data mismatch! The file read back is different from the original.")
	} else {
		t.Log("SUCCESS: Data read back matches perfectly.")
	}

	// Cleanup (Release/Close handle)
	jfs.Release(ctx, entry.Inode, fh)
}

func TestJuiceFSForceIMAPRead(t *testing.T) {
	utils.SetLogLevel(logrus.InfoLevel)
	configPath := os.Getenv("TEST_CONFIG")
	if configPath == "" {
		t.Skip("TEST_CONFIG not set")
	}

	dbPath := "imap_read_test.db"
	metaPath := "imap_read_meta.db"
	os.Remove(dbPath)
	os.Remove(metaPath)
	defer os.Remove(dbPath)
	defer os.Remove(metaPath)

	cfg, ok := loadConfigFromFile(configPath, dbPath)
	if !ok {
		t.Fatalf("Failed to load config")
	}

	// 1. Setup VFS and Write File
	blobStore, _ := mailfs.NewMailFS(cfg)
	metaConf := meta.Config{Retries: 10}
	m := meta.NewClient("sqlite3://"+metaPath, &metaConf)
	_ = m.Init(&meta.Format{Name: "testfs", UUID: "test-uuid", Storage: "mailfs", BlockSize: 4096}, true)

	chunkConf := chunk.Config{
		BlockSize: 10 * 1024 * 1024,
		MaxUpload: 1,
	}
	chunkStore := chunk.NewCachedStore(blobStore, chunkConf, nil)
	jfs := vfs.NewVFS(&vfs.Config{Chunk: &chunkConf, Meta: &metaConf}, m, chunkStore, nil, nil)

	ctx := &TestContext{Context: meta.NewContext(0, 0, []uint32{0})}
	var data []byte
	testFileSource := os.Getenv("TEST_FILE_TO_WRITE")

	if testFileSource != "" {
		var err error
		data, err = os.ReadFile(testFileSource)
		if err != nil {
			t.Fatalf("Failed to read TEST_FILE_TO_WRITE: %v", err)
		}
		t.Logf("Using data from file: %s (%d bytes)", testFileSource, len(data))
	} else {
		fileSize := 1 * 1024 * 1024 // 1MB for speed
		t.Logf("Generating %d bytes of random data...", fileSize)
		data = make([]byte, fileSize)
		rand.Seed(time.Now().UnixNano())
		rand.Read(data)
	}
	fileSize := len(data)

	t.Log("Writing file to MailFS...")
	entry, fh, _ := jfs.Create(ctx, 1, "imap_test.bin", 0644, 022, uint32(os.O_RDWR))
	_ = jfs.Write(ctx, entry.Inode, data, 0, fh)
	_ = jfs.Flush(ctx, entry.Inode, fh, 0)
	jfs.Release(ctx, entry.Inode, fh)

	t.Log("--- Write complete. Purging local caches... ---")

	// 2. PURGE: Delete local DB data to force IMAP fetch
	conn, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("failed to open sqlite: %v", err)
	}
	_, err = conn.Exec("DELETE FROM blob_data")
	if err != nil {
		t.Fatalf("Failed to clear local DB: %v", err)
	}
	conn.Close()

	// 3. Re-initialize strictly for Reading
	// We create a FRESH MailFS and ChunkStore so they have no memory cache.
	newBlobStore, _ := mailfs.NewMailFS(cfg)

	newChunkStore := chunk.NewCachedStore(newBlobStore, chunkConf, nil)
	newJfs := vfs.NewVFS(&vfs.Config{Chunk: &chunkConf, Meta: &metaConf}, m, newChunkStore, nil, nil)

	t.Log("--- Reading back from IMAP (Local data purged) ---")
	readBuf := make([]byte, fileSize)

	// Open the file again
	entry2, fh2, errCode := newJfs.Open(ctx, entry.Inode, uint32(os.O_RDONLY))
	if errCode != 0 {
		t.Fatalf("VFS Open failed: %v", errCode)
	}

	_, rErr := newJfs.Read(ctx, entry2.Inode, readBuf, 0, fh2)
	if rErr != 0 {
		t.Fatalf("VFS Read failed: %v", rErr)
	}

	if !bytes.Equal(data, readBuf) {
		t.Fatal("DATA MISMATCH! The data returned from IMAP does not match the original.")
	}
	t.Log("SUCCESS: Data successfully fetched from IMAP and verified.")
	newJfs.Release(ctx, entry2.Inode, fh2)
}

func bytesEqual(a, b []byte) bool {
	return bytes.Equal(a, b)
}
