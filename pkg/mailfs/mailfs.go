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
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/juicedata/juicefs/pkg/object"
	"github.com/juicedata/juicefs/pkg/utils"
	_ "github.com/mattn/go-sqlite3"
	"github.com/revv00/mailfs/pkg/config"
)

var logger = utils.GetLogger("mailfs")

type DefaultObjectStorage struct{}

func (s DefaultObjectStorage) Create(ctx context.Context) error {
	return nil
}

func (s DefaultObjectStorage) Limits() object.Limits {
	return object.Limits{IsSupportMultipartUpload: false, IsSupportUploadPartCopy: false}
}

func (s DefaultObjectStorage) Head(ctx context.Context, key string) (object.Object, error) {
	return nil, errors.New("not supported")
}

func (s DefaultObjectStorage) Get(ctx context.Context, key string, off, limit int64, getters ...object.AttrGetter) (io.ReadCloser, error) {
	return nil, errors.New("not supported")
}

func (s DefaultObjectStorage) Put(ctx context.Context, key string, in io.Reader, getters ...object.AttrGetter) error {
	return errors.New("not supported")
}

func (s DefaultObjectStorage) Copy(ctx context.Context, dst, src string) error {
	return errors.New("not supported")
}

func (s DefaultObjectStorage) Delete(ctx context.Context, key string, getters ...object.AttrGetter) error {
	return errors.New("not supported")
}

func (s DefaultObjectStorage) CreateMultipartUpload(ctx context.Context, key string) (*object.MultipartUpload, error) {
	return nil, errors.New("not supported")
}

func (s DefaultObjectStorage) UploadPart(ctx context.Context, key string, uploadID string, num int, body []byte) (*object.Part, error) {
	return nil, errors.New("not supported")
}

func (s DefaultObjectStorage) UploadPartCopy(ctx context.Context, key string, uploadID string, num int, srcKey string, off, size int64) (*object.Part, error) {
	return nil, errors.New("not supported")
}

func (s DefaultObjectStorage) AbortUpload(ctx context.Context, key string, uploadID string) {}

func (s DefaultObjectStorage) CompleteUpload(ctx context.Context, key string, uploadID string, parts []*object.Part) error {
	return errors.New("not supported")
}

func (s DefaultObjectStorage) ListUploads(ctx context.Context, marker string) ([]*object.PendingPart, string, error) {
	return nil, "", nil
}

func (s DefaultObjectStorage) List(ctx context.Context, prefix, start, token, delimiter string, limit int64, followLink bool) ([]object.Object, bool, string, error) {
	return nil, false, "", errors.New("not supported")
}

func (s DefaultObjectStorage) ListAll(ctx context.Context, prefix, marker string, followLink bool) (<-chan object.Object, error) {
	return nil, errors.New("not supported")
}

type mailObject struct {
	key   string
	size  int64
	mtime time.Time
	isDir bool
}

func (o *mailObject) Key() string          { return o.key }
func (o *mailObject) Size() int64          { return o.size }
func (o *mailObject) Mtime() time.Time     { return o.mtime }
func (o *mailObject) IsDir() bool          { return o.isDir }
func (o *mailObject) IsSymlink() bool      { return false }
func (o *mailObject) StorageClass() string { return "" }

// IMAP ID command (RFC 2971)
type imapIDCommand struct {
	Details []interface{}
}

func init() {
	object.Register("mailfs", func(endpoint, accessKey, secretKey, token string) (object.ObjectStorage, error) {
		accounts, err := config.LoadAccountsFromJSON(endpoint)
		if err != nil {
			return nil, fmt.Errorf("load mailfs accounts: %s", err)
		}

		if err := config.ValidateAccounts(accounts); err != nil {
			return nil, fmt.Errorf("validate accounts: %s", err)
		}

		cfg := config.MailFSConfig{
			Accounts:   accounts,
			DBPath:     "mailfs-metadata.db",
			BlobFolder: "juicefs-blobs",
		}

		return NewMailFS(cfg)
	})
}

func (c *imapIDCommand) Command() *imap.Command {
	return &imap.Command{
		Name:      "ID",
		Arguments: []interface{}{c.Details},
	}
}

// Helper struct to satisfy the RespHandler interface
type genericHandler struct {
	handle func(imap.Resp) error
}

func (h genericHandler) Handle(resp imap.Resp) error {
	return h.handle(resp)
}

type mailBlob struct {
	key            string
	size           int64
	mtime          time.Time
	data           []byte
	account        int    // primary account index
	replicaAccount int    // replica account index
	msgID          string // primary message ID
	replicaMsgID   string // replica message ID
}

// safeIMAPClient wraps the imap client with a mutex because Selecting a mailbox
// alters the connection state, which is not safe for concurrent use.
type safeIMAPClient struct {
	sync.Mutex
	c *client.Client
}

type mailFS struct {
	sync.RWMutex
	DefaultObjectStorage // Embeds the interface implementation

	config      config.MailFSConfig
	db          *sql.DB
	accounts    []*config.MailAccount
	imapClients []*safeIMAPClient
	blobCache   map[string]*mailBlob // in-memory hot cache
}

// NewMailFS creates a new mailFS instance
func NewMailFS(cfg config.MailFSConfig) (*mailFS, error) {
	// Validate configuration
	if len(cfg.Accounts) == 0 {
		return nil, errors.New("at least one email account is required")
	}

	if cfg.DBPath == "" {
		cfg.DBPath = "mailfs.db"
	}
	if cfg.ReplicationFactor == 0 {
		cfg.ReplicationFactor = 1
	}
	if cfg.ReplicationFactor > len(cfg.Accounts) {
		return nil, fmt.Errorf("replication factor (%d) cannot exceed number of accounts (%d)", cfg.ReplicationFactor, len(cfg.Accounts))
	}

	// Initialize SQLite database
	db, err := sql.Open("sqlite3", cfg.DBPath)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite db: %w", err)
	}

	mfs := &mailFS{
		config:      cfg,
		db:          db,
		accounts:    cfg.Accounts,
		imapClients: make([]*safeIMAPClient, len(cfg.Accounts)),
		blobCache:   make(map[string]*mailBlob),
	}

	if err := mfs.initDB(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialize db: %w", err)
	}

	// Initialize IMAP connections
	if err := mfs.initIMAPConnections(); err != nil {
		logger.Warnf("Issues initializing IMAP connections: %v", err)
	}

	logger.Infof("MailFS initialized with %d email accounts", len(mfs.config.Accounts))
	return mfs, nil
}

// initDB initializes SQLite schema
func (m *mailFS) initDB() error {
	schema := `
	CREATE TABLE IF NOT EXISTS blobs (
		key TEXT PRIMARY KEY,
		size INTEGER,
		mtime INTEGER,
		account INTEGER,
		replica_account INTEGER,
		msg_id TEXT,
		replica_msg_id TEXT,
		created_at INTEGER
	);
	
	CREATE TABLE IF NOT EXISTS blob_data (
		key TEXT PRIMARY KEY,
		data TEXT
	);
	`
	_, err := m.db.Exec(schema)
	return err
}

func (m *mailFS) initIMAPConnections() error {
	skipVerify := os.Getenv("MAILFS_SKIP_TLS_VERIFY") == "1"

	// Legacy CipherSuites required for older mail providers (Sina, 163, etc.)
	// Go 1.18+ disables these by default, so we must explicitly enable them.
	legacyCiphers := []uint16{
		tls.TLS_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_RSA_WITH_AES_256_CBC_SHA,
		tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
		tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
		tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
		tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
	}

	for i, acc := range m.accounts {
		logger.Debugf("[IMAP] Connecting to %s (%s)", acc.IMAPHost, acc.Email)

		host, port, err := net.SplitHostPort(acc.IMAPHost)
		if err != nil {
			host = acc.IMAPHost
			port = "993"
		}
		addr := net.JoinHostPort(host, port)

		tlsConfig := &tls.Config{
			ServerName:         host,
			InsecureSkipVerify: skipVerify,
			MinVersion:         tls.VersionTLS10, // Support older servers
			CipherSuites:       legacyCiphers,    // Enable RSA ciphers
		}

		var c *client.Client

		// 1. Try Dialing with Implicit TLS (Usually port 993)
		dialer := &net.Dialer{Timeout: 15 * time.Second}
		conn, err := tls.DialWithDialer(dialer, "tcp", addr, tlsConfig)

		if err != nil {
			logger.Debugf("[IMAP] TLS handshake failed for %s: %v. Trying STARTTLS on port 143...", acc.Email, err)

			// 2. Fallback to Port 143 + STARTTLS
			plainAddr := net.JoinHostPort(host, "143")
			plainConn, pErr := net.DialTimeout("tcp", plainAddr, 15*time.Second)
			if pErr != nil {
				logger.Warnf("[IMAP] Connection failed for %s on both 993 and 143", acc.Email)
				continue
			}

			c, err = client.New(plainConn)
			if err == nil {
				err = c.StartTLS(tlsConfig)
			}

			if err != nil {
				logger.Warnf("[IMAP] STARTTLS failed for %s: %v", acc.Email, err)
				if c != nil {
					c.Close()
				}
				continue
			}
		} else {
			// Successfully connected via DialTLS
			c, err = client.New(conn)
			if err != nil {
				logger.Warnf("[IMAP] Failed to create client for %s: %v", acc.Email, err)
				conn.Close()
				continue
			}
		}

		// Login with Authorization Code (授权码)
		if err := c.Login(acc.Email, acc.Password); err != nil {
			c.Close()
			logger.Warnf("[IMAP] Login failed for %s: %v. Check if using App Authorization Code.", acc.Email, err)
			continue
		}

		// NetEase (163/126) and some others require ID command to avoid "Unsafe Login" error
		// especially when Select is called later.
		idCmd := &imapIDCommand{
			Details: []interface{}{"name", "JuiceFS", "version", "1.0.0"},
		}
		if _, err := c.Execute(idCmd, nil); err != nil {
			logger.Debugf("[IMAP] ID command failed for %s (might not be supported): %v", acc.Email, err)
		}

		// Ensure the storage folder exists
		if err := c.Create(acc.Folder); err != nil {
			// Folder might already exist
		}

		m.imapClients[i] = &safeIMAPClient{c: c}
		logger.Infof("[IMAP] Successfully connected and logged in: %s", acc.Email)
	}
	return nil
}

func (m *mailFS) String() string {
	return fmt.Sprintf("mailfs://%d-accounts/", len(m.accounts))
}

func (m *mailFS) Limits() object.Limits {
	return object.Limits{
		IsSupportMultipartUpload: false,
		IsSupportUploadPartCopy:  false,
		MinPartSize:              1024,
		MaxPartSize:              20 * 1024 * 1024,
	}
}

func (m *mailFS) Create(ctx context.Context) error { return nil }

// hashToAccount is a helper to distribute keys across accounts.
// It is exported implicitly to the test file in the same package.
func hashToAccount(key string, numAccounts int) int {
	if numAccounts == 0 {
		return 0
	}
	h := 0
	for _, c := range key {
		h = 31*h + int(c)
	}
	if h < 0 {
		h = -h
	}
	return h % numAccounts
}

func (m *mailFS) getReplicaAccounts(key string) (int, int) {
	numAccounts := len(m.accounts)
	if numAccounts == 0 {
		return -1, -1
	}

	primary := hashToAccount(key, numAccounts)
	if numAccounts == 1 {
		return primary, primary
	}

	replica := (primary + 1) % numAccounts
	return primary, replica
}

func (m *mailFS) lockAccounts(indices ...int) func() {
	var sorted []int
	unique := make(map[int]bool)
	for _, idx := range indices {
		if idx >= 0 && idx < len(m.accounts) && !unique[idx] {
			unique[idx] = true
			sorted = append(sorted, idx)
		}
	}
	sort.Ints(sorted)

	for _, idx := range sorted {
		m.accounts[idx].Lock() // Lock the MailAccount value
	}

	return func() {
		for i := len(sorted) - 1; i >= 0; i-- {
			m.accounts[sorted[i]].Unlock() // Unlock the MailAccount value
		}
	}
}

func (m *mailFS) Get(ctx context.Context, key string, off, limit int64, getters ...object.AttrGetter) (io.ReadCloser, error) {
	m.RLock()
	// 1. Memory Cache
	if blob, ok := m.blobCache[key]; ok {
		m.RUnlock()
		return m.readRange(blob.data, off, limit), nil
	}
	m.RUnlock()

	// 2. Check Database for Metadata AND Data
	var primaryIdx int
	var replicaIdx sql.NullInt64
	var primaryMsgID string
	var replicaMsgID sql.NullString
	var encodedData sql.NullString

	row := m.db.QueryRowContext(ctx,
		`SELECT account, replica_account, msg_id, replica_msg_id, data 
		 FROM blobs 
		 LEFT JOIN blob_data ON blobs.key = blob_data.key 
		 WHERE blobs.key = ?`, key)

	err := row.Scan(&primaryIdx, &replicaIdx, &primaryMsgID, &replicaMsgID, &encodedData)
	if err == sql.ErrNoRows {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}

	var data []byte

	// 3. If Data in Local DB, use it
	if encodedData.Valid && encodedData.String != "" {
		data, err = base64.StdEncoding.DecodeString(encodedData.String)
		if err == nil {
			logger.Infof("Loaded blob %s from local cache database", key)
			return m.readRange(data, off, limit), nil
		}
		logger.Warnf("Failed to decode local data for %s, falling back to IMAP: %v", key, err)
	}

	// 4. Fallback: Fetch from IMAP
	unlock := m.lockAccounts(primaryIdx)
	data, err = m.fetchFromEmail(ctx, primaryIdx, primaryMsgID, key)
	unlock()

	if err != nil {
		if replicaIdx.Valid {
			repIdx := int(replicaIdx.Int64)
			logger.Warnf("Primary fetch failed for %s (acc %d), trying replica (acc %d): %v", key, primaryIdx, repIdx, err)
			unlockRep := m.lockAccounts(repIdx)
			data, err = m.fetchFromEmail(ctx, repIdx, replicaMsgID.String, key)
			unlockRep()
		}
	}

	if err != nil {
		return nil, fmt.Errorf("failed to retrieve blob %s from any source: %w", key, err)
	}

	return m.readRange(data, off, limit), nil
}

func (m *mailFS) readRange(data []byte, off, limit int64) io.ReadCloser {
	if off > int64(len(data)) {
		off = int64(len(data))
	}
	data = data[off:]
	if limit > 0 && limit < int64(len(data)) {
		data = data[:limit]
	}
	return io.NopCloser(bytes.NewBuffer(append([]byte{}, data...)))
}

func (m *mailFS) Put(ctx context.Context, key string, in io.Reader, getters ...object.AttrGetter) error {
	if key == "" {
		return errors.New("key cannot be empty")
	}

	data, err := io.ReadAll(in)
	if err != nil {
		return err
	}

	primaryIdx, replicaIdx := m.getReplicaAccounts(key)

	// Ensure serialized access to the accounts involved
	unlock := m.lockAccounts(primaryIdx, replicaIdx)
	defer unlock()

	// 1. Upload to Email (Primary)
	msgID, err := m.storeInEmail(ctx, primaryIdx, key, data)
	if err != nil {
		return fmt.Errorf("failed to store primary replica: %w", err)
	}

	// 2. Upload to Email (Replica) - Best effort
	var replicaMsgID string
	if replicaIdx != primaryIdx {
		rid, rErr := m.storeInEmail(ctx, replicaIdx, key, data)
		if rErr != nil {
			logger.Warnf("Replica upload failed for %s: %v", key, rErr)
		} else {
			replicaMsgID = rid
		}
	}

	// 3. Update Database (Metadata + Hot Cache)
	m.Lock()
	defer m.Unlock()

	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UnixNano()
	encodedData := base64.StdEncoding.EncodeToString(data)

	// Update Metadata
	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO blobs 
		(key, size, mtime, account, replica_account, msg_id, replica_msg_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		key, int64(len(data)), now, primaryIdx, replicaIdx, msgID, replicaMsgID, now)
	if err != nil {
		return err
	}

	// Store Data if small/needed (here we store everything in blob_data for simplicity)
	_, err = tx.ExecContext(ctx,
		`INSERT OR REPLACE INTO blob_data (key, data) VALUES (?, ?)`,
		key, encodedData)
	if err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	// 4. Update Memory Cache
	m.blobCache[key] = &mailBlob{
		key:            key,
		size:           int64(len(data)),
		mtime:          time.Unix(0, now),
		data:           data,
		account:        primaryIdx,
		replicaAccount: replicaIdx,
		msgID:          msgID,
		replicaMsgID:   replicaMsgID,
	}

	return nil
}

func (m *mailFS) storeInEmail(ctx context.Context, accountIdx int, key string, data []byte) (string, error) {
	if accountIdx >= len(m.accounts) {
		return "", fmt.Errorf("invalid account index %d", accountIdx)
	}
	acc := m.accounts[accountIdx]

	host, port, err := net.SplitHostPort(acc.SMTPHost)
	if err != nil {
		host = acc.SMTPHost
		port = "465" // Default to SSL port
	}
	addr := net.JoinHostPort(host, port)

	logger.Infof("[SMTP] Attempting connection to %s for blob %s", addr, key)

	tlsConfig := &tls.Config{
		ServerName:         host,
		InsecureSkipVerify: os.Getenv("MAILFS_SKIP_TLS_VERIFY") == "1",
		MinVersion:         tls.VersionTLS10,
		CipherSuites: []uint16{
			tls.TLS_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_RSA_WITH_AES_256_CBC_SHA,
			tls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		},
	}

	var c *smtp.Client
	// 1. If port is 465, use Implicit SSL (tls.Dial)
	if port == "465" {
		logger.Debugf("[SMTP] Using Implicit SSL for port 465")
		conn, err := tls.Dial("tcp", addr, tlsConfig)
		if err != nil {
			return "", fmt.Errorf("SMTP DialTLS failed: %w", err)
		}
		c, err = smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return "", fmt.Errorf("SMTP NewClient failed: %w", err)
		}
	} else {
		// 2. Use Plain connection + STARTTLS (Port 587/25)
		logger.Debugf("[SMTP] Using STARTTLS for port %s", port)
		c, err = smtp.Dial(addr)
		if err != nil {
			return "", fmt.Errorf("SMTP Dial failed: %w", err)
		}
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err = c.StartTLS(tlsConfig); err != nil {
				c.Close()
				return "", fmt.Errorf("SMTP STARTTLS failed: %w", err)
			}
		}
	}
	defer c.Quit()

	// 3. Authenticate
	logger.Debugf("[SMTP] Authenticating %s...", acc.Email)
	auth := smtp.PlainAuth("", acc.Email, acc.Password, host)
	if err = c.Auth(auth); err != nil {
		return "", fmt.Errorf("SMTP Auth failed: %w", err)
	}

	// 4. Construct Content
	boundary := fmt.Sprintf("JUICEFS_BOUNDARY_%d", time.Now().UnixNano())
	subject := fmt.Sprintf("JuiceFS Blob :%s", key)
	encodedBlob := base64.StdEncoding.EncodeToString(data)

	body := new(bytes.Buffer)
	fmt.Fprintf(body, "From: %s\r\n", acc.Email)
	fmt.Fprintf(body, "To: %s\r\n", acc.Email)
	fmt.Fprintf(body, "Subject: %s\r\n", subject)
	fmt.Fprintf(body, "X-JuiceFS-Key: %s\r\n", key)
	fmt.Fprintf(body, "MIME-Version: 1.0\r\n")
	fmt.Fprintf(body, "Content-Type: multipart/mixed; boundary=%s\r\n\r\n", boundary)

	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	body.WriteString("JuiceFS Blob Attachment\r\n")

	body.WriteString("--" + boundary + "\r\n")
	body.WriteString("Content-Type: application/octet-stream\r\n")
	body.WriteString("Content-Transfer-Encoding: base64\r\n")
	fmt.Fprintf(body, "Content-Disposition: attachment; filename=\"%s.bin\"\r\n\r\n", key)
	body.WriteString(encodedBlob)
	body.WriteString("\r\n--" + boundary + "--\r\n")

	// 5. Send Data
	logger.Debugf("[SMTP] Sending MAIL FROM...")
	if err = c.Mail(acc.Email); err != nil {
		return "", fmt.Errorf("MAIL FROM failed: %w", err)
	}

	logger.Debugf("[SMTP] Sending RCPT TO...")
	if err = c.Rcpt(acc.Email); err != nil {
		return "", fmt.Errorf("RCPT TO failed: %w", err)
	}

	logger.Debugf("[SMTP] Sending DATA...")
	w, err := c.Data()
	if err != nil {
		return "", fmt.Errorf("DATA command failed: %w", err)
	}

	if _, err = w.Write(body.Bytes()); err != nil {
		return "", fmt.Errorf("writing body failed: %w", err)
	}

	if err = w.Close(); err != nil {
		return "", fmt.Errorf("closing data writer failed: %w", err)
	}

	logger.Infof("[SMTP] Successfully stored blob %s in email", key)
	return "", nil
}

func (m *mailFS) fetchFromEmail(ctx context.Context, accountIdx int, msgID string, key string) ([]byte, error) {
	if accountIdx >= len(m.imapClients) || m.imapClients[accountIdx] == nil {
		return nil, errors.New("IMAP client not initialized")
	}

	safeClient := m.imapClients[accountIdx]

	safeClient.Lock()
	defer safeClient.Unlock()

	c := safeClient.c

	// Use the client-side search logic verified in the Delete function.
	// This ensures consistency: if Delete can find it, Fetch can find it.
	seqNum, err := m.findMsgUIDClientSide(ctx, c, key)
	if err != nil {
		return nil, fmt.Errorf("search failed: %w", err)
	}
	if seqNum == 0 {
		return nil, os.ErrNotExist
	}

	// Now that we have the sequence number, fetch the full body.
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(seqNum)

	section := &imap.BodySectionName{}
	items := []imap.FetchItem{section.FetchItem()}

	messages := make(chan *imap.Message, 1)
	done := make(chan error, 1)

	go func() {
		done <- c.Fetch(seqSet, items, messages)
	}()

	msg := <-messages
	if err := <-done; err != nil {
		return nil, err
	}
	if msg == nil {
		return nil, os.ErrNotExist
	}

	r := msg.GetBody(section)
	if r == nil {
		return nil, errors.New("empty body")
	}

	fullBody, _ := io.ReadAll(r)

	// Existing parsing logic to extract the specific blob part
	parts := bytes.Split(fullBody, []byte("Content-Transfer-Encoding: base64"))
	if len(parts) < 2 {
		return nil, errors.New("could not find base64 part in email")
	}

	blobPart := parts[1]
	// Skip potential headers in the same part (like Content-Disposition)
	if headerEnd := bytes.Index(blobPart, []byte("\r\n\r\n")); headerEnd != -1 {
		blobPart = blobPart[headerEnd+4:]
	} else if headerEnd := bytes.Index(blobPart, []byte("\n\n")); headerEnd != -1 {
		blobPart = blobPart[headerEnd+2:]
	} else {
		blobPart = bytes.TrimLeft(blobPart, "\r\n")
	}

	if idx := bytes.Index(blobPart, []byte("--JUICEFS_BOUNDARY")); idx != -1 {
		blobPart = blobPart[:idx]
	}

	// Remove all whitespace (including newlines, spaces, etc)
	cleanBlob := bytes.NewBuffer(make([]byte, 0, len(blobPart)))
	for _, b := range blobPart {
		if b != '\r' && b != '\n' && b != ' ' && b != '\t' {
			cleanBlob.WriteByte(b)
		}
	}

	decoded, err := base64.StdEncoding.DecodeString(cleanBlob.String())
	if err != nil {
		return nil, fmt.Errorf("base64 decode failed for key %s: %w", key, err)
	}

	return decoded, nil
}

func (m *mailFS) rawSearchSubject(c *client.Client, key string) ([]uint32, error) {
	cmd := &imap.Command{
		Name: "SEARCH",
		Arguments: []interface{}{
			"SUBJECT",
			key,
		},
	}

	var uids []uint32

	// Handler for the untagged SEARCH response
	handler := genericHandler{
		handle: func(resp imap.Resp) error {
			// We cast to *imap.DataResp to access fields
			data, ok := resp.(*imap.DataResp)
			if !ok {
				return nil
			}

			// Look for responses like: * SEARCH 123 456
			if len(data.Fields) > 0 {
				if op, ok := data.Fields[0].(string); ok && op == "SEARCH" {
					for _, field := range data.Fields[1:] {
						if num, err := imap.ParseNumber(field); err == nil {
							uids = append(uids, num)
						}
					}
				}
			}
			return nil
		},
	}

	// Execute the command
	status, err := c.Execute(cmd, handler)
	if err != nil {
		return nil, err
	}

	// Check for protocol-level errors (NO/BAD) using string literals
	if status.Type == "NO" || status.Type == "BAD" {
		return nil, fmt.Errorf("server rejected search: %v", status.Info)
	}

	return uids, nil
}

// Helper function to find a message by key
func (m *mailFS) findMsgUID(ctx context.Context, c *client.Client, key string) (uint32, error) {
	// 1. SELECT INBOX
	_, err := c.Select("INBOX", false)
	if err != nil {
		return 0, fmt.Errorf("select failed: %w", err)
	}

	// 2. Try Raw Server-Side Search
	ids, err := m.rawSearchSubject(c, key)
	if err != nil {
		logger.Warnf("Raw search failed: %v", err)
		// Fallback code would go here
	}

	// 3. Fallback: If 0 results, search for broad "JuiceFS" string
	if len(ids) == 0 {
		ids, err = m.rawSearchSubject(c, "JuiceFS")
		if err != nil {
			logger.Warnf("Broad search failed: %v", err)
		}
	}

	if len(ids) == 0 {
		return 0, nil // Really not found
	}

	// 4. Client-Side Verification
	seqSet := new(imap.SeqSet)
	seqSet.AddNum(ids...) // Pass the slice using variadic operator

	section := &imap.BodySectionName{BodyPartName: imap.BodyPartName{Specifier: imap.HeaderSpecifier}}
	items := []imap.FetchItem{section.FetchItem()}

	messages := make(chan *imap.Message, len(ids))
	done := make(chan error, 1)

	go func() {
		done <- c.Fetch(seqSet, items, messages)
	}()

	var confirmedUID uint32
	for msg := range messages {
		if msg.Envelope == nil {
			// If Envelope is nil, parse the header section manually
			r := msg.GetBody(section)
			if r != nil {
				buf, _ := io.ReadAll(r)
				if strings.Contains(string(buf), key) {
					confirmedUID = msg.SeqNum
				}
			}
			continue
		}

		// If Envelope is present (some servers send it automatically)
		if strings.Contains(msg.Envelope.Subject, key) {
			confirmedUID = msg.SeqNum
		}
	}

	if err := <-done; err != nil {
		return 0, err
	}

	return confirmedUID, nil
}

func (m *mailFS) findMsgUIDClientSide0(ctx context.Context, c *client.Client, key string) (uint32, error) {
	// 1. SELECT INBOX
	// We must select INBOX because that is where SMTP delivers new blobs.
	mbox, err := c.Select("INBOX", false)
	if err != nil {
		return 0, fmt.Errorf("select INBOX failed: %w", err)
	}

	if mbox.Messages == 0 {
		return 0, nil
	}

	logger.Infof("Found %d messages in INBOX", mbox.Messages)
	// 2. CALCULATE RANGE (Last 1000 Messages)
	// If you have 5000 emails, we fetch 4001 -> 5000.
	// If you have 500 emails, we fetch 1 -> 500.
	const searchDepth = 1000
	from := uint32(1)
	if mbox.Messages > searchDepth {
		from = mbox.Messages - searchDepth + 1
	}
	to := mbox.Messages

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, to)

	// 3. FETCH ENVELOPES ONLY
	// imap.FetchEnvelope is lightweight. It gets Subject, Date, From, etc.
	// It does NOT download the body or attachments.
	items := []imap.FetchItem{imap.FetchEnvelope}

	// Channel buffer matches our search depth to prevent blocking
	messages := make(chan *imap.Message, searchDepth)
	done := make(chan error, 1)

	go func() {
		done <- c.Fetch(seqSet, items, messages)
	}()

	var foundSeqNum uint32

	// 4. CLIENT-SIDE MATCHING
	// We iterate through the fetched messages in Go.
	for msg := range messages {
		if msg.Envelope == nil {
			continue
		}

		// Check the Subject string.
		// Since you save blobs as "JuiceFS Blob :<key>", checking for the key here is safe.
		// strings.Contains is fast and avoids all server-side tokenization issues.
		logger.Infof("Checking message SeqNum %d with msg: %v", msg.SeqNum, msg)
		if strings.Contains(msg.Envelope.Subject, key) {
			// Found it! We update foundSeqNum.
			// We keep looping to find the *highest* SeqNum (most recent) if duplicates exist.
			foundSeqNum = msg.SeqNum
		}
	}

	// Check if the network fetch itself failed
	if err := <-done; err != nil {
		return 0, fmt.Errorf("fetch failed: %w", err)
	}

	if foundSeqNum == 0 {
		logger.Debugf("Key %s not found in the last %d messages", key, searchDepth)
		return 0, nil
	}

	logger.Infof("Client-side search found key %s at SeqNum %d", key, foundSeqNum)
	return foundSeqNum, nil
}

func (m *mailFS) findMsgUIDClientSide(ctx context.Context, c *client.Client, key string) (uint32, error) {
	// 1. SELECT INBOX
	mbox, err := c.Select("INBOX", false)
	if err != nil {
		return 0, fmt.Errorf("select INBOX failed: %w", err)
	}

	if mbox.Messages == 0 {
		return 0, nil
	}

	// 2. CALCULATE RANGE (Last 1000)
	const searchDepth = 1000
	from := uint32(1)
	if mbox.Messages > searchDepth {
		from = mbox.Messages - searchDepth + 1
	}
	to := mbox.Messages

	seqSet := new(imap.SeqSet)
	seqSet.AddRange(from, to)

	// 3. DEFINE FETCH ITEM: Raw Subject Header Only
	// We do NOT use FetchEnvelope anymore. We ask for the raw bytes of the Subject.
	section := &imap.BodySectionName{
		BodyPartName: imap.BodyPartName{
			Specifier: imap.HeaderSpecifier,
			Fields:    []string{"Subject"}, // Only fetch Subject to save bandwidth
		},
		Peek: true, // "PEEK" prevents marking the email as Read/Seen
	}

	items := []imap.FetchItem{section.FetchItem()}

	messages := make(chan *imap.Message, searchDepth)
	done := make(chan error, 1)

	go func() {
		done <- c.Fetch(seqSet, items, messages)
	}()

	var foundSeqNum uint32

	// 4. PARSE RAW HEADERS
	for msg := range messages {
		// Get the body section (Reader)
		r := msg.GetBody(section)
		if r == nil {
			continue
		}
		logger.Infof("Checking message SeqNum %d with msg: %v", msg.SeqNum, msg)

		// Read the raw bytes (e.g., "Subject: JuiceFS Blob :my-key\r\n")
		headerBytes, _ := io.ReadAll(r)
		headerStr := string(headerBytes)

		// logger.Infof("Checking msg %d raw header: %q", msg.SeqNum, headerStr)

		// Simple string check
		if strings.Contains(headerStr, key) {
			foundSeqNum = msg.SeqNum
		}
	}

	if err := <-done; err != nil {
		return 0, fmt.Errorf("fetch failed: %w", err)
	}

	if foundSeqNum == 0 {
		return 0, nil
	}

	logger.Infof("Found key %s at SeqNum %d", key, foundSeqNum)
	return foundSeqNum, nil
}

// Simple helper for Go < 1.18
func contains(s, substr string) bool {
	// import "strings"
	return strings.Contains(s, substr)
}

func (m *mailFS) Delete(ctx context.Context, key string, getters ...object.AttrGetter) error {
	m.Lock()
	defer m.Unlock()

	// 1. Find account info from DB metadata
	var primaryIdx int
	var replicaIdx sql.NullInt64
	err := m.db.QueryRowContext(ctx, "SELECT account, replica_account FROM blobs WHERE key = ?", key).Scan(&primaryIdx, &replicaIdx)

	if err != nil {
		if err == sql.ErrNoRows {
			logger.Infof("[DELETE] Failed to find blob %s in database, assuming already deleted", key)
			return nil // Key not in DB, nothing to delete
		}
		logger.Infof("[DELETE] Failed due to %v", err)
		return err
	}

	// 2. Physical Deletion from Email Accounts
	accountsToDelete := []int{primaryIdx}
	if replicaIdx.Valid && int(replicaIdx.Int64) != primaryIdx {
		accountsToDelete = append(accountsToDelete, int(replicaIdx.Int64))
	}

	unlock := m.lockAccounts(accountsToDelete...)
	defer unlock()

	for _, idx := range accountsToDelete {
		if idx >= len(m.imapClients) || m.imapClients[idx] == nil {
			continue
		}

		// Run deletion for this account
		err := func(accIdx int) error {
			safeClient := m.imapClients[accIdx]
			acc := m.accounts[accIdx]

			safeClient.Lock()
			defer safeClient.Unlock()

			c := safeClient.c
			id, err := m.findMsgUIDClientSide(ctx, c, key)
			if err != nil {
				logger.Errorf("[DELETE] Search error for %s: %v", key, err)
				return err
			}

			if id == 0 {
				logger.Warnf("[DELETE] Blob %s not found on server %s", key, acc.Email)
				return nil
			}
			ids := []uint32{id}

			seqSet := new(imap.SeqSet)
			seqSet.AddNum(ids...)
			item := imap.FormatFlagsOp(imap.AddFlags, true)
			flags := []interface{}{imap.DeletedFlag}

			if err := c.Store(seqSet, item, flags, nil); err != nil {
				return fmt.Errorf("marking deleted failed: %w", err)
			}

			if err := c.Expunge(nil); err != nil {
				return fmt.Errorf("expunge failed: %w", err)
			}

			logger.Infof("[DELETE] Successfully wiped blob %s from %s", key, acc.Email)
			return nil
		}(idx)

		if err != nil {
			logger.Errorf("[DELETE] Error deleting from account %d: %v", idx, err)
		}
	}

	// 3. Final Step: Cleanup local metadata and cache
	delete(m.blobCache, key)
	_, _ = m.db.ExecContext(ctx, "DELETE FROM blobs WHERE key = ?", key)
	_, _ = m.db.ExecContext(ctx, "DELETE FROM blob_data WHERE key = ?", key)

	return nil
}

func (m *mailFS) Head(ctx context.Context, key string) (object.Object, error) {
	m.RLock()
	defer m.RUnlock()

	if blob, ok := m.blobCache[key]; ok {
		return &mailObject{
			key:   blob.key,
			size:  blob.size,
			mtime: blob.mtime,
			isDir: false,
		}, nil
	}

	var size int64
	var mtime int64
	err := m.db.QueryRowContext(ctx,
		`SELECT size, mtime FROM blobs WHERE key = ?`, key).Scan(&size, &mtime)

	if err == sql.ErrNoRows {
		return nil, os.ErrNotExist
	}
	if err != nil {
		return nil, err
	}

	return &mailObject{
		key:   key,
		size:  size,
		mtime: time.Unix(0, mtime),
		isDir: false,
	}, nil
}

func (m *mailFS) Copy(ctx context.Context, dst, src string) error {
	d, err := m.Get(ctx, src, 0, -1)
	if err != nil {
		return err
	}
	defer d.Close()
	return m.Put(ctx, dst, d)
}

func (m *mailFS) List(ctx context.Context, prefix, marker, token, delimiter string, limit int64, followLink bool) ([]object.Object, bool, string, error) {
	m.RLock()
	defer m.RUnlock()

	var objs []object.Object
	rows, err := m.db.QueryContext(ctx,
		`SELECT key, size, mtime FROM blobs WHERE key >= ? AND key LIKE ? ORDER BY key LIMIT ?`,
		marker, prefix+"%", limit+1)
	if err != nil {
		return nil, false, "", err
	}
	defer rows.Close()

	for rows.Next() {
		var key string
		var size int64
		var mtime int64
		rows.Scan(&key, &size, &mtime)
		objs = append(objs, &mailObject{
			key:   key,
			size:  size,
			mtime: time.Unix(0, mtime),
			isDir: false,
		})
	}

	var nextMarker string
	hasMore := false
	if len(objs) > int(limit) {
		hasMore = true
		objs = objs[:len(objs)-1]
		nextMarker = objs[len(objs)-1].Key()
	}

	return objs, hasMore, nextMarker, nil
}

func (m *mailFS) ListAll(ctx context.Context, prefix, marker string, followLink bool) (<-chan object.Object, error) {
	ch := make(chan object.Object)
	go func() {
		defer close(ch)
		objs, _, _, _ := m.List(ctx, prefix, marker, "", "", 1000000, false)
		for _, o := range objs {
			ch <- o
		}
	}()
	return ch, nil
}

func (m *mailFS) Close() error {
	m.Lock()
	defer m.Unlock()
	for _, sc := range m.imapClients {
		if sc != nil && sc.c != nil {
			sc.c.Logout()
		}
	}
	return m.db.Close()
}
