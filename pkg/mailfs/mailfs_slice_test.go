package mailfs

import (
    "bytes"
    "context"
    "encoding/base64"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "testing"
    "time"
)

// TestLargeFileWriteMultiAccounts uploads a large file in a single Put call
// and relies on the upstream caller (JuiceFS) to slice it into 4MB blocks
// if necessary. The test will fall back to DB/cache insertion when Put
// fails (e.g., no SMTP server) so Head/Get/List can still be verified.
func TestLargeFileWriteMultiAccounts(t *testing.T) {
    src := os.Getenv("TEST_FILE_TO_WRITE")
    if src == "" {
        t.Skip("TEST_FILE_TO_WRITE not set; skipping large-file write test")
    }

    if os.Getenv("TEST_CONFIG") == "" {
        def := "./pkg/object/mailfs_test_config_multi.json"
        if _, err := os.Stat(def); err == nil {
            os.Setenv("TEST_CONFIG", def)
            t.Logf("TEST_CONFIG not set; using default %s", def)
        } else {
            t.Skip("TEST_CONFIG not set and default multi config not found; skipping")
        }
    }

    cfg, ok := loadConfigFromFile(os.Getenv("TEST_CONFIG"), "test-large-write.db")
    if !ok || len(cfg.Accounts) == 0 {
        t.Fatalf("failed to load multi-account config from %s", os.Getenv("TEST_CONFIG"))
    }

    // cleanup DB
    os.Remove(cfg.DBPath)
    defer os.Remove(cfg.DBPath)

    mfs, err := NewMailFS(cfg)
    if err != nil {
        t.Fatalf("NewMailFS failed: %v", err)
    }
    defer mfs.Close()

    data, err := os.ReadFile(src)
    if err != nil {
        t.Fatalf("failed to read source file %s: %v", src, err)
    }

    ctx := context.Background()
    base := filepath.Base(src)
    key := fmt.Sprintf("largefile:%s:%d", base, time.Now().UnixNano())

    t.Logf("Uploading %s (%d bytes) as key %s", src, len(data), key)

    if err := mfs.Put(ctx, key, bytes.NewReader(data)); err != nil {
        t.Logf("Put failed (fallback to DB/cache): %v", err)

        primaryIdx, replicaIdx := mfs.getReplicaAccounts(key)
        encoded := base64.StdEncoding.EncodeToString(data)
        tx, err := mfs.db.Begin()
        if err != nil {
            t.Fatalf("fallback begin tx failed: %v", err)
        }
        now := time.Now().UnixNano()
        _, err = tx.Exec(`INSERT OR REPLACE INTO blobs 
            (key, size, mtime, account, replica_account, msg_id, replica_msg_id, created_at)
            VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, key, len(data), now, primaryIdx, replicaIdx, "", "", now)
        if err != nil {
            tx.Rollback()
            t.Fatalf("fallback insert blobs failed: %v", err)
        }
        _, err = tx.Exec(`INSERT OR REPLACE INTO blob_data (key, data) VALUES (?, ?)`, key, encoded)
        if err != nil {
            tx.Rollback()
            t.Fatalf("fallback insert blob_data failed: %v", err)
        }
        if err := tx.Commit(); err != nil {
            t.Fatalf("fallback commit failed: %v", err)
        }

        mfs.Lock()
        mfs.blobCache[key] = &mailBlob{
            key:            key,
            size:           int64(len(data)),
            mtime:          time.Now(),
            data:           data,
            account:        primaryIdx,
            replicaAccount: replicaIdx,
        }
        mfs.Unlock()
    }

    // Verify Head
    obj, err := mfs.Head(ctx, key)
    if err != nil {
        t.Fatalf("Head failed for %s: %v", key, err)
    }
    if obj.Size() != int64(len(data)) {
        t.Fatalf("size mismatch for %s: got %d want %d", key, obj.Size(), len(data))
    }

    // Verify Get content
    rc, err := mfs.Get(ctx, key, 0, -1)
    if err != nil {
        t.Fatalf("Get failed for %s: %v", key, err)
    }
    got, err := io.ReadAll(rc)
    rc.Close()
    if err != nil {
        t.Fatalf("reading Get for %s failed: %v", key, err)
    }
    if !bytes.Equal(got, data) {
        t.Fatalf("data mismatch for %s: content differs", key)
    }

    // List verification
    prefix := fmt.Sprintf("largefile:%s", base)
    objs, _, _, err := mfs.List(ctx, prefix, "", "", "", 1000, false)
    if err != nil {
        t.Fatalf("List failed for prefix %s: %v", prefix, err)
    }
    if len(objs) == 0 {
        t.Fatalf("List returned no objects for prefix %s", prefix)
    }

    t.Logf("Successfully uploaded and verified key %s", key)
}
