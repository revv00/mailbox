package main

import (
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"os/exec"

	"github.com/revv00/mailfs/pkg/config"
	"github.com/revv00/mailfs/pkg/mbox"
	"github.com/revv00/mailfs/pkg/version"
	"github.com/urfave/cli/v2"
	"golang.org/x/term"
)

func main() {
	commonFlags := []cli.Flag{
		&cli.StringFlag{
			Name:    "account",
			Aliases: []string{"a"},
			Usage:   "Specify account config name (in ~/.mbox/) or path",
			Value:   "accounts",
		},
		&cli.IntFlag{
			Name:    "parallel",
			Aliases: []string{"j"},
			Usage:   "Parallelism level (number of concurrent connections)",
			Value:   1,
		},
		&cli.BoolFlag{
			Name:  "parallel-by-provider",
			Usage: "Limit parallelism to one connection per email provider",
		},
		&cli.DurationFlag{
			Name:    "timeout",
			Aliases: []string{"t"},
			Usage:   "Timeout for uploading each block (e.g. 5m, 10m)",
			Value:   10 * time.Minute,
		},
		&cli.BoolFlag{
			Name:  "remove-sent",
			Usage: "Remove sent emails from Sent folder after upload to save space",
			Value: true,
		},
	}

	app := &cli.App{
		Name:                   "mbox",
		Usage:                  "Infinite USB Stick over Email",
		Version:                fmt.Sprintf("%s (%s)", version.Revision, version.RevisionDate),
		UseShortOptionHandling: true,
		Flags: append([]cli.Flag{
			&cli.BoolFlag{
				Name:  "embed-data",
				Usage: "embed raw file data into the .mbox (makes it huge)",
			},
		}, commonFlags...),
		Commands: []*cli.Command{
			{
				Name:      "config",
				Usage:     "Setup email accounts",
				ArgsUsage: "[account_name]",
				Action: func(c *cli.Context) error {
					accName := c.Args().First()
					if accName == "" {
						configs := listConfigs()
						if len(configs) > 0 {
							if len(configs) > 1 || (len(configs) == 1 && configs[0] != "accounts") {
								fmt.Println("Found existing account configurations:")
								for i, name := range configs {
									fmt.Printf("%d) %s\n", i+1, name)
								}
								fmt.Print("Select one by number, or enter a new name [default: accounts]: ")
								var input string
								_, _ = fmt.Scanln(&input)
								input = strings.TrimSpace(input)
								if input != "" {
									var idx int
									if _, err := fmt.Sscanf(input, "%d", &idx); err == nil && idx > 0 && idx <= len(configs) {
										accName = configs[idx-1]
									} else {
										accName = input
									}
								}
							}
						}
					}
					if accName == "" {
						accName = getAccount(c)
					}

					cfgPath := getConfigPath(accName)
					var initialAccounts []*config.MailAccount
					var replication int
					var subjectPrefix string
					var removeSent bool = true // Default for new configs
					var isNew bool

					if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
						isNew = true
						fmt.Printf("Configuration for '%s' not found. Creating a new one...\n", accName)
					} else {
						isEnc, _ := config.IsEncrypted(cfgPath)
						var parsed *config.ParsedConfig
						var err error
						if isEnc {
							// For config command, we handle password prompt manually if name is custom
							masterPwd, err := getMasterPassword(c)
							if err != nil {
								return err
							}
							parsed, err = config.LoadAccountsFromEncryptedJSON(cfgPath, masterPwd)
						} else {
							parsed, err = config.LoadAccountsFromJSON(cfgPath)
						}
						if err != nil {
							return err
						}
						initialAccounts = parsed.Accounts
						replication = parsed.Replication
						subjectPrefix = parsed.SubjectPrefix
						removeSent = parsed.RemoveSent
					}

					// Create temp file for editing
					tmpFile, err := os.CreateTemp("", "mbox-config-*.json")
					if err != nil {
						return err
					}
					defer os.Remove(tmpFile.Name())

					if initialAccounts != nil {
						data, _ := config.SerializeAccounts(initialAccounts, replication, subjectPrefix, removeSent)
						_ = os.WriteFile(tmpFile.Name(), data, 0600)
					} else {
						_ = config.GenerateConfigTemplate(tmpFile, 3)
					}
					tmpFile.Close()

					// Open Editor
					editor := os.Getenv("EDITOR")
					if editor == "" {
						if runtime.GOOS == "windows" {
							editor = "notepad"
						} else {
							editor = "nano"
							if _, err := exec.LookPath(editor); err != nil {
								editor = "vi"
							}
						}
					}

					cmd := exec.Command(editor, tmpFile.Name())
					cmd.Stdin = os.Stdin
					cmd.Stdout = os.Stdout
					cmd.Stderr = os.Stderr
					if err := cmd.Run(); err != nil {
						return fmt.Errorf("editor failed: %w", err)
					}

					// Load back
					newAccounts, err := config.LoadAccountsFromJSON(tmpFile.Name())
					if err != nil {
						return fmt.Errorf("failed to parse updated config: %w", err)
					}

					// Prompt for Master Password to encrypt
					fmt.Println("\nConfiguration updated.")
					var masterPwd string
					if isNew {
						masterPwd, err = getNewMasterPassword(c)
					} else {
						masterPwd, err = getMasterPassword(c)
					}
					if err != nil {
						return err
					}

					_ = os.MkdirAll(filepath.Dir(cfgPath), 0755)
					if err := config.SaveAccountsEncrypted(cfgPath, newAccounts.Accounts, newAccounts.Replication, newAccounts.SubjectPrefix, newAccounts.RemoveSent, masterPwd); err != nil {
						return err
					}

					fmt.Printf("Success! Configuration encrypted and saved at %s\n", cfgPath)
					return nil
				},
			},
			{
				Name:   "ls",
				Usage:  "List cloud .mbox files",
				Flags:  commonFlags,
				Action: doLs,
			},
			{
				Name:  "stash",
				Usage: "Save files with 1x replication",
				Flags: append([]cli.Flag{
					&cli.StringFlag{Name: "password", Aliases: []string{"p"}},
				}, commonFlags...),
				Action: func(c *cli.Context) error {
					return doPut(c, 1)
				},
			},
			{
				Name:  "put",
				Usage: "Save files with 2x replication",
				Flags: append([]cli.Flag{
					&cli.StringFlag{Name: "password", Aliases: []string{"p"}},
				}, commonFlags...),
				Action: func(c *cli.Context) error {
					return doPut(c, 2)
				},
			},
			{
				Name:  "get",
				Usage: "Retrieve files from a stick file",
				Flags: append([]cli.Flag{
					&cli.StringFlag{Name: "password", Aliases: []string{"p"}},
				}, commonFlags...),
				Action: doGet,
			},
			{
				Name:  "del",
				Usage: "Delete a stick file (local only? or remote?)",
				Flags: commonFlags,
				Action: func(c *cli.Context) error {
					// "mbox del" is ambiguous in prompt.
					// Assuming deleting the local stick file and possibly attempting to wipe blobs?
					// Wiping blobs requires the DB inside the stick file.
					// Let's implement full wipe.
					return doDelete(c)
				},
			},
			{
				Name:   "wipe",
				Usage:  "DANGER: Wipe ALL blobs and configs from ALL configured accounts",
				Flags:  commonFlags,
				Action: doWipe,
			},
		},
	}

	if err := app.Run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func getConfigPath(name string) string {
	if name == "" {
		name = "accounts"
	}
	// If it's a path (contains / or begins with .), use it as-is
	if strings.Contains(name, string(os.PathSeparator)) || strings.HasPrefix(name, ".") || filepath.IsAbs(name) {
		return name
	}
	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".mbox", name)
}

func getAccount(c *cli.Context) string {
	acc := c.String("account")
	if !c.IsSet("account") {
		lineage := c.Lineage()
		found := false
		for _, ctx := range lineage {
			if ctx.IsSet("account") {
				acc = ctx.String("account")
				found = true
				break
			}
		}
		if !found {
			// Fallback 2: Manual parsing for trailing flags in c.Args()
			for i, arg := range c.Args().Slice() {
				if (arg == "-a" || arg == "--account") && i+1 < c.Args().Len() {
					acc = c.Args().Get(i + 1)
					found = true
					break
				}
			}
			if !found {
				// Fallback 3: Check os.Args directly if it hasn't been found yet
				for i := 1; i < len(os.Args)-1; i++ {
					if os.Args[i] == "-a" || os.Args[i] == "--account" {
						acc = os.Args[i+1]
						found = true
						break
					}
				}
			}
		}
	}
	if acc == "" {
		return "accounts"
	}
	return acc
}

func listConfigs() []string {
	home, _ := os.UserHomeDir()
	configDir := filepath.Join(home, ".mbox")
	matches, _ := filepath.Glob(filepath.Join(configDir, "*.json"))
	var names []string
	for _, path := range matches {
		name := filepath.Base(path)
		name = strings.TrimSuffix(name, ".json")
		names = append(names, name)
	}
	return names
}

func getMasterPassword(c *cli.Context) (string, error) {
	if p := os.Getenv("MBOX_MASTER_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Print("Enter Master Password: ")
	bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	return string(bytePassword), nil
}

func getNewMasterPassword(c *cli.Context) (string, error) {
	if p := os.Getenv("MBOX_MASTER_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Print("Set Master Password: ")
	bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	pwd := string(bytePassword)

	fmt.Print("Confirm Master Password: ")
	bytePasswordConfirm, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	if pwd != string(bytePasswordConfirm) {
		return "", fmt.Errorf("passwords do not match")
	}

	return pwd, nil
}

func getParallel(c *cli.Context) int {
	parallel := c.Int("parallel")
	if !c.IsSet("parallel") {
		// Fallback 1: check global context
		lineage := c.Lineage()
		if len(lineage) > 1 && lineage[1].IsSet("parallel") {
			parallel = lineage[1].Int("parallel")
		} else {
			// Fallback 2: Manual parsing for trailing flags in c.Args()
			for i, arg := range c.Args().Slice() {
				if (arg == "-j" || arg == "--parallel") && i+1 < c.Args().Len() {
					var val int
					fmt.Sscanf(c.Args().Get(i+1), "%d", &val)
					if val > 0 {
						parallel = val
					}
				}
			}
			// Fallback 3: Check os.Args directly if it hasn't been found yet
			for i := 1; i < len(os.Args)-1; i++ {
				if os.Args[i] == "-j" || os.Args[i] == "--parallel" {
					var val int
					fmt.Sscanf(os.Args[i+1], "%d", &val)
					if val > 0 {
						parallel = val
					}
				}
			}
		}
	}
	return parallel
}

func getTimeout(c *cli.Context) time.Duration {
	timeout := c.Duration("timeout")
	if !c.IsSet("timeout") {
		// Fallback 1: check global context
		lineage := c.Lineage()
		if len(lineage) > 1 && lineage[1].IsSet("timeout") {
			timeout = lineage[1].Duration("timeout")
		} else {
			// Fallback 2: Manual parsing for trailing flags in c.Args()
			for i, arg := range c.Args().Slice() {
				if (arg == "-t" || arg == "--timeout") && i+1 < c.Args().Len() {
					if dur, err := time.ParseDuration(c.Args().Get(i + 1)); err == nil {
						timeout = dur
					}
				}
			}
			// Fallback 3: Check os.Args directly
			for i := 1; i < len(os.Args)-1; i++ {
				if os.Args[i] == "-t" || os.Args[i] == "--timeout" {
					if dur, err := time.ParseDuration(os.Args[i+1]); err == nil {
						timeout = dur
					}
				}
			}
		}
	}
	return timeout
}

func getCacheDir(target string) string {
	home, _ := os.UserHomeDir()
	targetID := getTargetID(target)
	dir := filepath.Join(home, ".mbox", "state", targetID)
	_ = os.MkdirAll(dir, 0755)
	return dir
}

func getArchivePassword(c *cli.Context, confirm bool, fallback string) (string, error) {
	p := c.String("password")
	if p == "" {
		// Fallback: check c.Args() for trailing -p or --password
		for i, arg := range c.Args().Slice() {
			if (arg == "-p" || arg == "--password") && i+1 < c.Args().Len() {
				p = c.Args().Get(i + 1)
				break
			}
		}
		if p == "" {
			// Fallback: check os.Args directly
			for i := 1; i < len(os.Args)-1; i++ {
				if os.Args[i] == "-p" || os.Args[i] == "--password" {
					p = os.Args[i+1]
					break
				}
			}
		}
	}

	if p != "" {
		return p, nil
	}
	if p := os.Getenv("MBOX_ARCHIVE_PASSWORD"); p != "" {
		return p, nil
	}
	fmt.Print("Enter Archive Password (leave empty to use Master Password): ")
	bytePassword, err := term.ReadPassword(int(os.Stdin.Fd()))
	fmt.Println()
	if err != nil {
		return "", err
	}
	pwd := string(bytePassword)

	if pwd == "" {
		if fallback != "" {
			fmt.Println("Using Master Password.")
			return fallback, nil
		}
		// If no fallback provided (or unencrypted config), try to request Master Password
		// This ensures we always have *some* security or link to identity.
		fmt.Println("Using Master Password.")
		return getMasterPassword(c)
	}

	if confirm {
		fmt.Print("Confirm Archive Password: ")
		bytePasswordConfirm, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return "", err
		}
		if pwd != string(bytePasswordConfirm) {
			return "", fmt.Errorf("passwords do not match")
		}
	}

	return pwd, nil
}

func getParallelByProvider(c *cli.Context) bool {
	val := c.Bool("parallel-by-provider")
	if !val {
		// Fallback 1: check global context
		lineage := c.Lineage()
		if len(lineage) > 1 && lineage[1].IsSet("parallel-by-provider") {
			val = lineage[1].Bool("parallel-by-provider")
		} else {
			// Fallback 2: Manual parsing for trailing flags
			for _, arg := range c.Args().Slice() {
				if arg == "--parallel-by-provider" {
					return true
				}
			}
			// Fallback 3: Check os.Args directly
			for _, arg := range os.Args {
				if arg == "--parallel-by-provider" {
					return true
				}
			}
		}
	}
	return val
}

func getEmbedData(c *cli.Context) bool {
	val := c.Bool("embed-data")
	if !val {
		// Fallback 1: check global context
		lineage := c.Lineage()
		if len(lineage) > 1 && lineage[1].IsSet("embed-data") {
			val = lineage[1].Bool("embed-data")
		} else {
			// Fallback 2: Manual parsing for trailing flags
			for _, arg := range c.Args().Slice() {
				if arg == "--embed-data" {
					return true
				}
			}
			// Fallback 3: Check os.Args directly
			for _, arg := range os.Args {
				if arg == "--embed-data" {
					return true
				}
			}
		}
	}
	return val
}

func getRemoveSent(c *cli.Context) bool {
	// Defaults to true in flag definition, but we check IsSet for explicit overrides if needed.
	// Actually for BoolFlag, Value: true means it's true unless --remove-sent=false is passed.
	// urfave/cli v2 handles this.
	val := c.Bool("remove-sent")
	if !c.IsSet("remove-sent") {
		lineage := c.Lineage()
		if len(lineage) > 1 && lineage[1].IsSet("remove-sent") {
			val = lineage[1].Bool("remove-sent")
		} else {
			// Manual parsing for trailing flags
			for _, arg := range c.Args().Slice() {
				if arg == "--remove-sent=false" {
					return false
				}
				if arg == "--remove-sent" {
					return true
				}
			}
			// Check os.Args
			for _, arg := range os.Args {
				if arg == "--remove-sent=false" {
					return false
				}
				if arg == "--remove-sent" {
					return true
				}
			}
		}
	}
	return val
}

func loadGlobalAccounts(c *cli.Context) (*config.ParsedConfig, string, error) {
	cfgPath := getConfigPath(getAccount(c))
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return nil, "", fmt.Errorf("configuration file not found at %s. Run 'mbox config' first", cfgPath)
	}

	isEnc, err := config.IsEncrypted(cfgPath)
	if err != nil {
		return nil, "", err
	}

	var cfg *config.ParsedConfig
	var pwd string
	if isEnc {
		pwd, err = getMasterPassword(c)
		if err != nil {
			return nil, "", err
		}
		cfg, err = config.LoadAccountsFromEncryptedJSON(cfgPath, pwd)
	} else {
		fmt.Println("⚠️ Warning: Using unencrypted config file. Run 'mbox config' to encrypt it.")
		cfg, err = config.LoadAccountsFromJSON(cfgPath)
	}

	if err == nil {
		cfg.ParallelByProvider = getParallelByProvider(c)
		cfg.RemoveSent = getRemoveSent(c)
	}
	return cfg, pwd, err
}

func doPut(c *cli.Context, repl int) error {
	if c.NArg() < 1 {
		return fmt.Errorf("missing file/directory to upload")
	}
	target := c.Args().First()

	// 1. Load Global Config
	fmt.Println("[1/2] Unlock Identity")
	parsed, masterPwd, err := loadGlobalAccounts(c)
	if err != nil {
		return err
	}

	fmt.Println("[2/2] Encrypt Archive")
	archivePwd, err := getArchivePassword(c, true, masterPwd)
	if err != nil {
		return err
	}

	// Override replication factor if specified by command (stash=1, put=2)
	if repl > 0 {
		parsed.Replication = repl
	}
	if parsed.Replication <= 0 {
		parsed.Replication = 1
	}

	// Generate a unique ID for this archive based on the target for isolation
	targetID := getTargetID(target)
	if !strings.HasSuffix(parsed.SubjectPrefix, ":") && !strings.HasSuffix(parsed.SubjectPrefix, " ") {
		parsed.SubjectPrefix += " "
	}
	parsed.SubjectPrefix = fmt.Sprintf("%s%s:", parsed.SubjectPrefix, targetID)
	fmt.Printf("Using unique subject prefix for isolation: %s\n", parsed.SubjectPrefix)

	// 2. Prepare Environment (Persistent Cache for Resumable Uploads)
	cacheDir := getCacheDir(target)
	dbPath := filepath.Join(cacheDir, "mailfs.db")

	// 3. Init Client
	// If the database already exists, we skip formatting to enable resume
	isNew := false
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		isNew = true
	}
	// Default to NoCache (true) unless --embed-data is provided
	// Init Client with parallelism (check local flag first, then global)
	parallel := getParallel(c)
	if parallel > 5 {
		fmt.Printf("Capping upload parallelism to 5 (requested: %d)\n", parallel)
		parallel = 5
	}
	timeout := getTimeout(c)
	fmt.Printf("Parallelism: %d, Timeout: %v, Accounts: %d, Replication: %d\n", parallel, timeout, len(parsed.Accounts), parsed.Replication)

	client, err := mbox.NewMBoxClient(dbPath, parsed, !c.Bool("embed-data"), parallel, timeout)
	if err != nil {
		return err
	}

	if err := client.Init(isNew); err != nil {
		return fmt.Errorf("failed to init fs: %w", err)
	}

	// 4. Import
	vPath := "/" + filepath.Base(target)
	fmt.Printf("Uploading %s to %s...\n", target, vPath)
	if err := client.Import(target, vPath); err != nil {
		client.Close()
		return fmt.Errorf("import failed: %w", err)
	}

	// 5. Pack
	// We must close the databases before packing to ensure all data is flushed and files are stable.
	if err := client.CloseDB(); err != nil {
		return fmt.Errorf("failed to close databases: %w", err)
	}
	outputFile := filepath.Base(target) + ".mbox"
	fmt.Printf("Packing to %s...\n", outputFile)

	// Since we might have encrypted the config file, we need the Raw JSON to put inside the .mbox for portability.
	// We'll regenerate the JSON from the accounts we loaded (which are now in memory decrypted).
	// This ensures the .mbox stays portable with its own crypto.
	cfgData, err := config.SerializeAccounts(parsed.Accounts, parsed.Replication, parsed.SubjectPrefix, parsed.RemoveSent)
	if err != nil {
		return err
	}

	if err := mbox.Pack(archivePwd, cfgData, dbPath, outputFile); err != nil {
		return fmt.Errorf("pack failed: %w", err)
	}

	// 6. Upload .mbox to first account
	fmt.Printf("Uploading %s to cloud...\n", outputFile)
	if err := client.UploadConfig(outputFile); err != nil {
		fmt.Printf("⚠️ Warning: Failed to upload .mbox to cloud: %v\n", err)
	}

	_ = client.Close()
	fmt.Println("Done!")
	return nil
}

func doGet(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("missing stick file")
	}
	targetArg := c.Args().First()
	stickFile := targetArg

	// 0. Check if local, otherwise try cloud
	if _, err := os.Stat(stickFile); os.IsNotExist(err) {
		fmt.Printf("Local file %s not found, searching cloud...\n", stickFile)

		parsed, _, err := loadGlobalAccounts(c)
		if err == nil {
			tmpDir, _ := os.MkdirTemp("", "mbox-search-*")
			defer os.RemoveAll(tmpDir)

			parallel := getParallel(c)
			timeout := getTimeout(c)
			fmt.Printf("Search Parallelism: %d\n", parallel)

			searchClient, err := mbox.NewMBoxClient(filepath.Join(tmpDir, "search.db"), parsed, false, parallel, timeout)
			if err == nil {
				defer searchClient.Close()
				matches, err := searchClient.SearchConfig(targetArg)
				if err == nil && len(matches) > 0 {
					if len(matches) == 1 {
						stickFile = matches[0]
						fmt.Printf("Found match: %s. Downloading...\n", stickFile)
						if err := searchClient.DownloadConfig(stickFile, stickFile); err != nil {
							return fmt.Errorf("download failed: %w", err)
						}
					} else {
						fmt.Println("Matches found in cloud:")
						for _, m := range matches {
							fmt.Printf(" - %s\n", m)
						}
						return fmt.Errorf("please specify the exact filename from the list above")
					}
				} else {
					return fmt.Errorf("file %s not found locally or in cloud", targetArg)
				}
			}
		} else {
			return fmt.Errorf("file %s not found and cloud search failed: %w", targetArg, err)
		}
	}

	archivePwd, err := getArchivePassword(c, false, "")
	if err != nil {
		return err
	}

	// 1. Unpack
	tmpDir, err := os.MkdirTemp("", "mbox-get-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	fmt.Printf("Unpacking %s...\n", stickFile)
	if err := mbox.Unpack(archivePwd, stickFile, tmpDir); err != nil {
		return fmt.Errorf("unpack failed: %w", err)
	}

	// 2. Load Config from Stick
	innerCfgPath := filepath.Join(tmpDir, "config.json")
	parsed, err := config.LoadAccountsFromJSON(innerCfgPath)
	if err != nil {
		return fmt.Errorf("failed to load inner config: %w", err)
	}

	// 3. Init Client with DB from Stick
	dbPath := filepath.Join(tmpDir, "mailfs.db")
	// For reading, NoCache=true means it will always check IMAP if it can't find it locally.
	// Since we want small .mbox by default, we always allow fetching from IMAP.
	parallel := getParallel(c)
	fmt.Printf("Parallelism: %d, Accounts: %d, Replication: %d\n", parallel, len(parsed.Accounts), parsed.Replication)

	client, err := mbox.NewMBoxClient(dbPath, parsed, !getEmbedData(c), parallel, getTimeout(c))
	if err != nil {
		return err
	}
	defer client.Close()
	// No Init/Format, just Load
	if err := client.Init(false); err != nil {
		return fmt.Errorf("failed to load fs: %w", err)
	}

	// 4. Export
	// We want to export whatever is in root (except . trash etc which JFS manages?)
	// Let's assume user wants to restore to CWD.
	cwd, _ := os.Getwd()
	fmt.Printf("Restoring to %s...\n", cwd)

	// Note: Our Export implementation in client.go takes (vPath, localPath)
	// We iterate root
	// Since client.go doesn't expose ReadDir easily in public API (I only added local Export),
	// I should probably simplify: Assume we uploaded ONE item at root level matching the stick file base name?
	// But users might rename stick file.
	// Best approach: List root and export everything.
	// Adding List to MBoxClient?
	// Or just "Export / ." (Recursive export of root to current dir)

	if err := client.Export("/", cwd); err != nil {
		return fmt.Errorf("export failed: %w", err)
	}

	fmt.Println("Done!")
	return nil
}

func doDelete(c *cli.Context) error {
	if c.NArg() < 1 {
		return fmt.Errorf("missing stick file to delete")
	}
	targetArg := c.Args().First()

	var targets []string
	isLocal := true

	// 1. Identify targets (Local vs Cloud)
	if _, err := os.Stat(targetArg); os.IsNotExist(err) {
		isLocal = false
		fmt.Printf("Local file %s not found, searching cloud...\n", targetArg)

		parsed, _, err := loadGlobalAccounts(c)
		if err != nil {
			return fmt.Errorf("failed to load global config for search: %w", err)
		}

		tmpDir, _ := os.MkdirTemp("", "mbox-search-*")
		defer os.RemoveAll(tmpDir)

		parallel := getParallel(c)
		timeout := getTimeout(c)
		fmt.Printf("Search Parallelism: %d\n", parallel)

		searchClient, err := mbox.NewMBoxClient(filepath.Join(tmpDir, "search.db"), parsed, false, parallel, timeout)
		if err != nil {
			return fmt.Errorf("search client init failed: %w", err)
		}

		matches, err := searchClient.SearchConfig(targetArg)
		searchClient.Close()

		if err != nil {
			return fmt.Errorf("search failed: %w", err)
		}
		if len(matches) == 0 {
			return fmt.Errorf("file %s not found locally or in cloud", targetArg)
		}
		targets = matches
	} else {
		targets = []string{targetArg}
	}

	// 2. Confirm
	if len(targets) > 1 {
		fmt.Printf("⚠️  DANGER: You are about to DELETE %d files and ALL their data from the cloud:\n", len(targets))
		for _, t := range targets {
			fmt.Printf(" - %s\n", t)
		}
	} else {
		fmt.Printf("⚠️  DANGER: You are about to DELETE %s and ALL its data from the cloud.\n", targets[0])
	}
	fmt.Printf("This action CANNOT be undone. Are you sure? (y/N): ")
	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "y" && confirm != "Y" {
		fmt.Println("Aborted.")
		return nil
	}

	// 3. Authenticate for Unpacking
	fmt.Println("Enter Password to decrypt the stick(s):")
	archivePwd, err := getArchivePassword(c, false, "")
	if err != nil {
		return err
	}

	// 4. Process each target
	for _, targetName := range targets {
		fmt.Printf("\n--- Processing %s ---\n", targetName)
		stickFile := targetName

		// Load global config for each to ensure fresh state (though we could optimize, this is safer)
		parsedGlobal, _, err := loadGlobalAccounts(c)
		if err != nil {
			fmt.Printf("Error: failed to load global config: %v\n", err)
			continue
		}

		unpackDir, err := os.MkdirTemp("", "mbox-del-*")
		if err != nil {
			return err
		}

		if !isLocal {
			fmt.Printf("Downloading metadata for %s...\n", targetName)
			dlClient, _ := mbox.NewMBoxClient(filepath.Join(unpackDir, "dl.db"), parsedGlobal, false, 1, 0)
			// Ensure it stays open long enough to download
			stickFile = filepath.Join(unpackDir, targetName)
			if err := dlClient.DownloadConfig(targetName, stickFile); err != nil {
				dlClient.Close()
				fmt.Printf("Error: download failed for %s: %v\n", targetName, err)
				continue
			}
			dlClient.Close()
		}

		// 5. Unpack to get Blob keys
		fmt.Printf("Unpacking stick metadata...\n")
		if err := mbox.Unpack(archivePwd, stickFile, unpackDir); err != nil {
			fmt.Printf("Error: unpack failed for %s: %v (Check password?)\n", targetName, err)
			continue
		}

		// 6. Init Client from Stick Config
		innerCfgPath := filepath.Join(unpackDir, "config.json")
		parsedInner, err := config.LoadAccountsFromJSON(innerCfgPath)
		if err != nil {
			fmt.Printf("Error: failed to load inner config: %v\n", err)
			continue
		}

		dbPath := filepath.Join(unpackDir, "mailfs.db")

		parallel := getParallel(c)
		fmt.Printf("Deletion Parallelism: %d\n", parallel)

		client, err := mbox.NewMBoxClient(dbPath, parsedInner, false, parallel, getTimeout(c))
		if err != nil {
			fmt.Printf("Error: client init failed: %v\n", err)
			continue
		}
		if err := client.Init(false); err != nil {
			fmt.Printf("Warning: failed to load FS meta: %v\n", err)
		}

		// 7. Delete Blobs
		fmt.Println("Deleting remote data chunks...")
		if err := client.DeleteAllBlobs(); err != nil {
			fmt.Printf("Error: blob deletion failed: %v\n", err)
		}

		// 8. Delete Remote Config
		remoteObjName := filepath.Base(targetName)
		fmt.Printf("Deleting remote stick file '%s'...\n", remoteObjName)
		if err := client.DeleteRemoteConfig(remoteObjName); err != nil {
			fmt.Printf("⚠️  Warning: Failed to delete remote .mbox file: %v\n", err)
		}

		client.Close()

		// 9. Delete Local File (if it started local)
		if isLocal {
			fmt.Printf("Deleting local file %s...\n", targetName)
			if err := os.Remove(targetName); err != nil {
				fmt.Printf("Error: failed to delete local file: %v\n", err)
			}
		}
	}

	fmt.Println("\nDeletion complete.")
	return nil
}

func doLs(c *cli.Context) error {
	// Load accounts (decrypted if necessary)
	parsed, _, err := loadGlobalAccounts(c)
	if err != nil {
		return err
	}

	// Create a temporary client just for listing
	// We use a temp dir for the DB
	tmpDir, err := os.MkdirTemp("", "mbox-ls-*")
	if err != nil {
		return fmt.Errorf("failed to create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	dbPath := filepath.Join(tmpDir, "ls.db")
	// NoCache=true, 1 replica (doesn't matter for list)
	parallel := getParallel(c)
	fmt.Printf("Scan Parallelism: %d\n", parallel)

	client, err := mbox.NewMBoxClient(dbPath, parsed, false, parallel, getTimeout(c))
	if err != nil {
		return fmt.Errorf("client init failed: %w", err)
	}
	defer client.Close()

	fmt.Println("Scanning first mail account for .mbox files...")
	matches, err := client.SearchConfig("*")
	if err != nil {
		return fmt.Errorf("list failed: %w", err)
	}

	if len(matches) == 0 {
		fmt.Println("No .mbox files found.")
	} else {
		for _, m := range matches {
			fmt.Println(m)
		}
	}
	return nil
}

func doWipe(c *cli.Context) error {
	// 1. Confirm
	fmt.Println("⚠️  DANGER: You are about to wipe ALL MailFS data from ALL configured accounts.")
	fmt.Println("This will delete all data chunks and all .mbox config files in the cloud.")
	fmt.Print("This action CANNOT be undone. Are you sure? (y/N): ")
	var confirm string
	fmt.Scanln(&confirm)
	if confirm != "y" && confirm != "Y" {
		fmt.Println("Aborted.")
		return nil
	}

	// 2. Load Global Config
	parsed, _, err := loadGlobalAccounts(c)
	if err != nil {
		return err
	}

	// 3. Init Client (We use a dummy DB since we only care about the IMAP wipe)
	tmpDir, _ := os.MkdirTemp("", "mbox-wipe-*")
	defer os.RemoveAll(tmpDir)
	dbPath := filepath.Join(tmpDir, "wipe.db")

	parallel := getParallel(c)
	fmt.Printf("Wipe Parallelism: %d\n", parallel)

	client, err := mbox.NewMBoxClient(dbPath, parsed, false, parallel, getTimeout(c))
	if err != nil {
		return fmt.Errorf("failed to init client: %w", err)
	}
	defer client.Close()

	fmt.Println("Starting global wipe...")
	if err := client.GlobalWipe(); err != nil {
		return fmt.Errorf("wipe failed: %w", err)
	}

	fmt.Println("Wipe complete. All cloud accounts have been cleared of MailFS data.")
	return nil
}

func getTargetID(target string) string {
	info, err := os.Stat(target)
	if err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())[:8]
	}

	h := md5.New()
	// Use name, size and modtime to create a reasonably unique ID for this archive session
	fmt.Fprintf(h, "%s%d%d", filepath.Base(target), info.Size(), info.ModTime().UnixNano())
	return fmt.Sprintf("%x", h.Sum(nil))[:8]
}
