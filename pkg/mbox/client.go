package mbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/juicedata/juicefs/pkg/chunk"
	"github.com/juicedata/juicefs/pkg/meta"
	"github.com/juicedata/juicefs/pkg/utils"
	"github.com/juicedata/juicefs/pkg/vfs"
	"github.com/revv00/mailfs/pkg/config"
	"github.com/revv00/mailfs/pkg/mailfs"
	"github.com/vbauerster/mpb/v7"
	"github.com/vbauerster/mpb/v7/decor"
	"golang.org/x/term"

	_ "github.com/mattn/go-sqlite3"
)

type DummyContext struct {
	context.Context
	UidVal uint32
	GidVal uint32
	PidVal uint32
}

func (c *DummyContext) Uid() uint32             { return c.UidVal }
func (c *DummyContext) Gid() uint32             { return c.GidVal }
func (c *DummyContext) Gids() []uint32          { return []uint32{c.GidVal} }
func (c *DummyContext) Pid() uint32             { return c.PidVal }
func (c *DummyContext) Cancel()                 {}
func (c *DummyContext) Canceled() bool          { return false }
func (c *DummyContext) CheckPermission() bool   { return false }
func (c *DummyContext) Duration() time.Duration { return 0 }
func (c *DummyContext) WithValue(k, v interface{}) meta.Context {
	// Return a new context wrapping the value, but we need to keep our methods.
	// For simplicity in this tool, we ignore values or return self (risky but okay for basic ops)
	return c
}

func NewContext(ctx context.Context) vfs.LogContext {
	if ctx == nil {
		ctx = context.Background()
	}
	return &DummyContext{
		Context: ctx,
		UidVal:  0,
		GidVal:  0,
		PidVal:  uint32(os.Getpid()),
	}
}

type slowRequestFilter struct {
	io.Writer
}

func (s *slowRequestFilter) Write(p []byte) (n int, err error) {
	if bytes.Contains(p, []byte("slow request:")) {
		return len(p), nil
	}
	return s.Writer.Write(p)
}

type MBoxClient struct {
	vfs      *vfs.VFS
	meta     meta.Meta
	auth     vfs.LogContext
	blob     *mailfs.MailFS
	parallel int
	sem      chan struct{}
	progress *mpb.Progress
	ctx      context.Context
	cancel   context.CancelFunc
}

func NewMBoxClient(dbPath string, conf *config.ParsedConfig, noCache bool, parallel int, putTimeout time.Duration) (*MBoxClient, error) {
	if parallel <= 0 {
		parallel = 1
	}
	if putTimeout <= 0 {
		putTimeout = 60 * time.Minute
	}
	cfg := config.MailFSConfig{
		Accounts:           conf.Accounts,
		DBPath:             filepath.Join(filepath.Dir(dbPath), "mailfs-blob.db"),
		ReplicationFactor:  conf.Replication,
		SubjectPrefix:      conf.SubjectPrefix,
		NoCache:            noCache,
		ParallelByProvider: conf.ParallelByProvider,
		RemoveSent:         conf.RemoveSent,
	}
	blob, err := mailfs.NewMailFS(cfg)
	if err != nil {
		return nil, fmt.Errorf("failed to init mailfs: %w", err)
	}
	// Calculate required Retries for JuiceFS vfs flush timeout.
	// JuiceFS wait time: (Retries + 2)^2 / 2 seconds.
	// We want wait >= putTimeout * 1.5 to ensure individual chunk timeouts don't trigger flush timeout prematurely.
	timeoutSec := putTimeout.Seconds()
	retries := int(math.Ceil(math.Sqrt(3.0*timeoutSec) - 2))
	if retries < 10 {
		retries = 10
	}

	// Suppress "slow request" warnings from juicefs chunk logger
	chunkLogger := utils.GetLogger("chunk")
	if _, ok := chunkLogger.Out.(*slowRequestFilter); !ok {
		chunkLogger.SetOutput(&slowRequestFilter{Writer: chunkLogger.Out})
	}

	// Use standard journal mode (not WAL) for portable archives.
	// This ensures all data is flushed to the main .db file when the database is closed,
	// allowing us to pack/tar the .db file safely. JuiceFS defaults to WAL if not specified.
	m := meta.NewClient(fmt.Sprintf("sqlite3://%s?_journal_mode=DELETE", dbPath), &meta.Config{
		Retries:     retries,
		MaxDeletes:  100,
		CaseInsensi: true,
		NoBGJob:     true,
		AtimeMode:   "noatime",
	})

	chunkConf := chunk.Config{
		BlockSize:  16 * 1024 * 1024,
		Compress:   "lz4",
		MaxUpload:  parallel,
		MaxRetries: 10,
		BufferSize: 300 << 20,
		GetTimeout: putTimeout,
		PutTimeout: putTimeout,
		Writeback:  false,
	}
	store := chunk.NewCachedStore(blob, chunkConf, nil)

	v := vfs.NewVFS(&vfs.Config{
		Meta:  &meta.Config{Retries: retries, MaxDeletes: 100},
		Chunk: &chunkConf,
		FuseOpts: &vfs.FuseOptions{
			EnableWriteback: false,
			MaxWrite:        64 * 1024 * 1024,
		},
	}, m, store, nil, nil)

	ctx, cancel := context.WithCancel(context.Background())

	mpbOpts := []mpb.ContainerOption{mpb.WithWidth(64)}
	if !term.IsTerminal(int(os.Stdout.Fd())) || os.Getenv("CI") != "" {
		mpbOpts = append(mpbOpts, mpb.WithOutput(io.Discard))
	}

	return &MBoxClient{
		vfs:      v,
		meta:     m,
		auth:     NewContext(context.Background()),
		blob:     blob,
		parallel: parallel,
		sem:      make(chan struct{}, parallel),
		progress: mpb.New(mpbOpts...),
		ctx:      ctx,
		cancel:   cancel,
	}, nil
}

func (c *MBoxClient) Init(format bool) error {
	if format {
		return c.meta.Init(&meta.Format{
			Name:        "mbox",
			UUID:        "mbox-uuid",
			Storage:     "mailfs",
			BlockSize:   4096,
			Compression: "lz4",
			Shards:      0,
		}, true)
	}
	_, err := c.meta.Load(false)
	return err
}

func (c *MBoxClient) CloseFS() error {
	_ = c.vfs.Flush(c.auth, 1, 0, 0)
	return c.meta.Shutdown()
}

func (c *MBoxClient) CloseDB() error {
	err1 := c.CloseFS() // This closes JuiceFS meta DB
	err2 := c.blob.CloseDB()
	if err1 != nil {
		return err1
	}
	return err2
}

func (c *MBoxClient) Close() error {
	c.cancel() // Cancel any ongoing operations
	c.progress.Wait()
	err1 := c.CloseFS()
	err2 := c.blob.Close()
	if err1 != nil {
		return err1
	}
	return err2
}

func (c *MBoxClient) UploadConfig(localPath string) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	// Upload to first account
	return c.blob.UploadMBox(filepath.Base(localPath), data)
}

func (c *MBoxClient) SearchConfig(pattern string) ([]string, error) {
	return c.blob.ListMBoxes(pattern)
}

func (c *MBoxClient) DownloadConfig(remoteName string, localPath string) error {
	data, err := c.blob.DownloadMBox(remoteName)
	if err != nil {
		return err
	}
	return os.WriteFile(localPath, data, 0600)
}

func (c *MBoxClient) DeleteRemoteConfig(filename string) error {
	return c.blob.DeleteMBox(filename)
}

func (c *MBoxClient) DeleteAllBlobs() error {
	objs, err := c.blob.ListAll("", "", false)
	if err != nil {
		return fmt.Errorf("listing blobs failed: %w", err)
	}

	var keys []string
	found := 0
	for obj := range objs {
		found++
		keys = append(keys, obj.Key())
	}

	if found == 0 {
		fmt.Println("No data chunks found in local metadata.")
		return nil
	}

	fmt.Printf("Found %d data chunks in local metadata. Starting batch deletion...\n", found)
	if err := c.blob.BatchDelete(keys); err != nil {
		return fmt.Errorf("batch deletion failed: %w", err)
	}
	return nil
}

func (c *MBoxClient) Import(localPath string, virtualPath string) error {
	info, err := os.Stat(localPath)
	if err != nil {
		return err
	}

	if info.IsDir() {
		type job struct {
			path, vPath string
		}
		jobs := make(chan job, 100)
		results := make(chan error, 1)
		var wg sync.WaitGroup

		// Start workers
		for i := 0; i < c.parallel; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-c.ctx.Done():
						return
					case j, ok := <-jobs:
						if !ok {
							return
						}
						if err := c.importFile(j.path, j.vPath); err != nil {
							c.cancel() // Stop all other workers
							select {
							case results <- err:
							default:
							}
						}
					}
				}
			}()
		}

		walkErr := filepath.Walk(localPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			select {
			case <-c.ctx.Done():
				return context.Canceled
			default:
			}
			rel, _ := filepath.Rel(localPath, path)
			if rel == "." {
				rel = ""
			}
			vPath := filepath.Join(virtualPath, rel)

			if info.IsDir() {
				return c.mkdirAll(vPath)
			}

			select {
			case jobs <- job{path, vPath}:
			case <-c.ctx.Done():
				return context.Canceled
			case err := <-results:
				return err
			}
			return nil
		})

		close(jobs)
		wg.Wait()

		if walkErr != nil {
			return walkErr
		}
		select {
		case err := <-results:
			return err
		case <-c.ctx.Done():
			return c.ctx.Err()
		default:
			return nil
		}
	}
	return c.importFile(localPath, virtualPath)
}

func (c *MBoxClient) mkdirAll(path string) error {
	path = filepath.Clean(path)
	if path == "/" || path == "." {
		return nil
	}

	dir := filepath.Dir(path)
	if dir != "/" && dir != "." {
		if err := c.mkdirAll(dir); err != nil {
			return err
		}
	}

	parentIno, err := c.lookupPath(filepath.Dir(path))
	if err != nil {
		return err
	}

	name := filepath.Base(path)
	var mode uint16 = 0755
	_, errno := c.vfs.Mkdir(c.auth, parentIno, name, mode, 022)
	if errno != 0 && errno != syscall.EEXIST {
		return fmt.Errorf("mkdir %s failed: %v", path, errno)
	}
	return nil
}

func (c *MBoxClient) importFile(localPath, vPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	info, _ := f.Stat()
	size := info.Size()

	if err := c.mkdirAll(filepath.Dir(vPath)); err != nil {
		return err
	}

	parentIno, err := c.lookupPath(filepath.Dir(vPath))
	if err != nil {
		return err
	}

	name := filepath.Base(vPath)
	var mode uint16 = 0644
	// Use O_CREATE without O_TRUNC to allow reusing existing chunk mappings for resumable uploads.
	// We'll call Truncate(size) at the end anyway to ensure the final file size is correct.
	entry, fh, errno := c.vfs.Create(c.auth, parentIno, name, mode, 022, uint32(os.O_WRONLY|os.O_CREATE))
	if errno != 0 {
		return fmt.Errorf("create %s failed: %v", vPath, errno)
	}

	const vfsWriteSize = 64 * 1024 * 1024 // Step by JuiceFS chunk size for optimal parallel chunking
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	var bar *mpb.Bar
	if !strings.HasPrefix(filepath.Base(localPath), ".") && size > 0 {
		bar = c.progress.AddBar(size,
			mpb.PrependDecorators(
				decor.Name(filepath.Base(localPath), decor.WC{W: len(filepath.Base(localPath)) + 1, C: decor.DidentRight}),
				decor.Counters(decor.UnitKiB, "% .2f / % .2f"),
			),
			mpb.AppendDecorators(
				decor.Percentage(decor.WC{W: 5}),
				decor.AverageSpeed(decor.UnitKiB, "% .2f", decor.WC{W: 12}),
			),
		)
	}

	for off := int64(0); off < size; off += vfsWriteSize {
		wg.Add(1)
		go func(offset int64) {
			defer wg.Done()

			c.sem <- struct{}{}
			defer func() { <-c.sem }()

			select {
			case <-c.ctx.Done():
				return
			default:
			}

			errMu.Lock()
			if firstErr != nil || c.ctx.Err() != nil {
				errMu.Unlock()
				return
			}
			errMu.Unlock()

			thisSize := int64(vfsWriteSize)
			if offset+thisSize > size {
				thisSize = size - offset
			}

			buf := make([]byte, thisSize)
			n, rErr := f.ReadAt(buf, offset)
			if rErr != nil && rErr != io.EOF {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("read %s at %d failed: %v", localPath, offset, rErr)
				}
				errMu.Unlock()
				return
			}

			if n > 0 {
				if failAfter := os.Getenv("MBOX_TEST_FAIL_AFTER"); failAfter != "" {
					var failOffset int64
					fmt.Sscanf(failAfter, "%d", &failOffset)
					if offset >= failOffset {
						panic("simulated failure")
					}
				}
				wErr := c.vfs.Write(c.auth, entry.Inode, buf[:n], uint64(offset), fh)
				if wErr == 0 {
					if bar != nil {
						bar.IncrBy(n)
					}
				}
				if wErr != 0 {
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("write %s at %d failed: %v", vPath, offset, wErr)
					}
					errMu.Unlock()
				}
			}
		}(off)
	}
	wg.Wait()

	if firstErr == nil && c.ctx.Err() != nil {
		firstErr = c.ctx.Err()
	}

	if firstErr != nil {
		if bar != nil {
			bar.Abort(true)
		}
		c.vfs.Release(c.auth, entry.Inode, fh)
		return firstErr
	}

	// Ensure buffers are flushed and handle is released
	_ = c.vfs.Flush(c.auth, entry.Inode, 0, fh)
	c.vfs.Release(c.auth, entry.Inode, fh)

	// Update metadata final size
	if errno := c.vfs.Truncate(c.auth, entry.Inode, size, 0, entry.Attr); errno != 0 {
		if bar != nil {
			bar.Abort(true)
		}
		return fmt.Errorf("truncate %s failed: %v", vPath, errno)
	}

	return nil
}

func (c *MBoxClient) Export(virtualPath string, localPath string) error {
	ino, err := c.lookupPath(virtualPath)
	if err != nil {
		return err
	}

	entry, errno := c.vfs.GetAttr(c.auth, ino, 0)
	if errno != 0 {
		return fmt.Errorf("getattr failed: %v", errno)
	}

	if entry.Attr.Typ == meta.TypeDirectory {
		return c.exportDir(ino, localPath)
	}

	return c.exportFile(ino, localPath, entry.Attr.Length)
}

func (c *MBoxClient) exportDir(ino vfs.Ino, localPath string) error {
	if err := os.MkdirAll(localPath, 0755); err != nil {
		return err
	}

	_, fh, errno := c.vfs.Open(c.auth, ino, uint32(os.O_RDONLY))
	if errno != 0 {
		return fmt.Errorf("opendir failed: %v", errno)
	}
	defer c.vfs.Release(c.auth, ino, fh)

	entries, _, errno := c.vfs.Readdir(c.auth, ino, 0, 0, fh, true)
	if errno != 0 {
		return fmt.Errorf("readdir failed: %v", errno)
	}

	type job struct {
		ino vfs.Ino
		lp  string
		sz  uint64
		typ uint8
	}
	jobs := make(chan job, 100)
	results := make(chan error, 1)
	var wg sync.WaitGroup

	for i := 0; i < c.parallel; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				var err error
				if j.typ == meta.TypeDirectory {
					err = c.exportDir(j.ino, j.lp)
				} else {
					err = c.exportFile(j.ino, j.lp, j.sz)
				}
				if err != nil {
					select {
					case results <- err:
					default:
					}
				}
			}
		}()
	}

	for _, e := range entries {
		name := string(e.Name)
		if name == "." || name == ".." {
			continue
		}
		newLocal := filepath.Join(localPath, name)

		select {
		case jobs <- job{e.Inode, newLocal, e.Attr.Length, e.Attr.Typ}:
		case err := <-results:
			close(jobs)
			wg.Wait()
			return err
		}
	}

	close(jobs)
	wg.Wait()

	select {
	case err := <-results:
		return err
	default:
		return nil
	}
}

func (c *MBoxClient) exportFile(ino vfs.Ino, localPath string, size uint64) error {
	dst, err := os.Create(localPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	entry, fh, errno := c.vfs.Open(c.auth, ino, uint32(os.O_RDONLY))
	if errno != 0 {
		return fmt.Errorf("open failed: %v", errno)
	}
	defer c.vfs.Release(c.auth, entry.Inode, fh)

	const vfsWriteSize = 64 * 1024 * 1024
	var wg sync.WaitGroup
	var errMu sync.Mutex
	var firstErr error

	var bar *mpb.Bar
	if !strings.HasPrefix(filepath.Base(localPath), ".") && size > 0 {
		bar = c.progress.AddBar(int64(size),
			mpb.PrependDecorators(
				decor.Name(filepath.Base(localPath), decor.WC{W: len(filepath.Base(localPath)) + 1, C: decor.DidentRight}),
				decor.Counters(decor.UnitKiB, "% .2f / % .2f"),
			),
			mpb.AppendDecorators(
				decor.Percentage(decor.WC{W: 5}),
				decor.AverageSpeed(decor.UnitKiB, "% .2f", decor.WC{W: 12}),
			),
		)
	}

	for off := uint64(0); off < size; off += uint64(vfsWriteSize) {
		wg.Add(1)
		go func(offset uint64) {
			defer wg.Done()

			c.sem <- struct{}{}
			defer func() { <-c.sem }()

			errMu.Lock()
			if firstErr != nil {
				errMu.Unlock()
				return
			}
			errMu.Unlock()

			thisSize := uint64(vfsWriteSize)
			if offset+thisSize > size {
				thisSize = size - offset
			}

			buf := make([]byte, thisSize)

			// Open a private handle for this chunk to avoid lock contention on shared fh
			_, cfh, rErrno := c.vfs.Open(c.auth, ino, uint32(os.O_RDONLY))
			if rErrno != 0 {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("open %s for read failed: %v", localPath, rErrno)
				}
				errMu.Unlock()
				return
			}
			defer c.vfs.Release(c.auth, ino, cfh)

			n, rErr := c.vfs.Read(c.auth, ino, buf, offset, cfh)
			if rErr == 0 || rErr == syscall.Errno(0) {
				if bar != nil {
					bar.IncrBy(int(n))
				}
			}
			if rErr != 0 && rErr != syscall.Errno(0) {
				errMu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("read %s at %d failed: %v", localPath, offset, rErr)
				}
				errMu.Unlock()
				return
			}
			if n > 0 {
				if _, wErr := dst.WriteAt(buf[:n], int64(offset)); wErr != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("write %s at %d failed: %v", localPath, offset, wErr)
					}
					errMu.Unlock()
				}
			}
		}(off)
	}
	wg.Wait()

	if firstErr != nil && bar != nil {
		bar.Abort(true)
	}

	return firstErr
}

func (c *MBoxClient) lookupPath(path string) (vfs.Ino, error) {
	if path == "/" {
		return 1, nil
	}
	path = filepath.Clean(path)
	dir, name := filepath.Split(path)
	if dir == "" || dir == "/" {
		node, errno := c.vfs.Lookup(c.auth, 1, name)
		if errno != 0 {
			return 0, fmt.Errorf("lookup %s failed: %v", name, errno)
		}
		return node.Inode, nil
	}

	parent, err := c.lookupPath(filepath.Dir(path))
	if err != nil {
		return 0, err
	}

	node, errno := c.vfs.Lookup(c.auth, parent, filepath.Base(path))
	if errno != 0 {
		return 0, fmt.Errorf("lookup %s failed: %v", path, errno)
	}
	return node.Inode, nil
}
func (c *MBoxClient) GlobalWipe() error {
	return c.blob.WipeAllCloudData()
}

type SliceInfo struct {
	meta.Slice
	PrimaryAccount int
	ReplicaAccount int
	PrimaryMsgID   string
	ReplicaMsgID   string
	PrimaryEmail   string
	ReplicaEmail   string
}

type ChunkInfo struct {
	Index  uint32
	Slices []SliceInfo
}

type MBoxFileInfo struct {
	Path   string
	Ino    vfs.Ino
	Size   uint64
	Chunks []ChunkInfo
}

func (c *MBoxClient) GetFileInfo(path string) (*MBoxFileInfo, error) {
	ino, err := c.lookupPath(path)
	if err != nil {
		return nil, err
	}

	attr, errno := c.vfs.GetAttr(c.auth, ino, 0)
	if errno != 0 {
		return nil, fmt.Errorf("getattr failed: %v", errno)
	}

	info := &MBoxFileInfo{
		Path: path,
		Ino:  ino,
		Size: attr.Attr.Length,
	}

	if attr.Attr.Typ != meta.TypeFile {
		return info, nil
	}

	for i := uint32(0); ; i++ {
		var slices []meta.Slice
		errno := c.meta.Read(c.auth, ino, i, &slices)
		if errno != 0 || (len(slices) == 0 && uint64(i)*64*1024*1024 >= info.Size) {
			break
		}
		if len(slices) == 0 {
			continue
		}

		cinfo := ChunkInfo{Index: i}
		for _, s := range slices {
			sinfo := SliceInfo{Slice: s}
			key, err := c.blob.FindKeyByChunkID(s.Id)
			if err == nil {
				p, r, pm, rm, err := c.blob.GetBlobInfo(key)
				if err == nil {
					sinfo.PrimaryAccount = p
					sinfo.ReplicaAccount = r
					sinfo.PrimaryMsgID = pm
					sinfo.ReplicaMsgID = rm
					sinfo.PrimaryEmail = c.blob.GetAccountEmail(p)
					sinfo.ReplicaEmail = c.blob.GetAccountEmail(r)
				}
			}
			cinfo.Slices = append(cinfo.Slices, sinfo)
		}
		info.Chunks = append(info.Chunks, cinfo)
	}

	return info, nil
}
