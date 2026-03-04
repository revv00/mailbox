# mbox: The Email USB Stick

A portable, secure, and extremely simple CLI tool to turn your email accounts into a distributed "USB stick" for file transfers and backups.

---

## 🛠 Installation
- Option 1: Download from [Releases](https://github.com/revv00/mailfs/releases).
- Option 2: If you have Go installed:
```bash
go install github.com/revv00/mailfs/cmd/mbox@latest
```

### Building from Source
Use the provided `Makefile` for optimized builds:
```bash
make       # Standard build (stripped, ~74MB)
make lite  # Optimized lite build (removes unused cloud drivers, ~25MB)
make clean # Remove built binaries
```
The `mbox.lite` version is recommended for maximum portability as it removes heavy cloud SDKs (S3, GCS, Azure, etc.) while keeping full support for Email-based storage.

For others, download the latest pre-built binary for your platform (Linux, macOS, Windows) from the [Releases](https://github.com/revv00/mailfs/releases) page.

---

## 📖 User Guide

### 1. Configuration
First, set up your email accounts:
```bash
./mbox config
# This creates ~/.mbox/accounts.json.tmp.
# 1. Edit the .tmp file with your IMAP/SMTP details.
# 2. Run './mbox config' again to encrypt it and set your new Master Password.
```
Your **Master Password** protects your email credentials so they aren't stored in plain text on your disk.

### 2. Stash (1× Replication – Quick Transfer)
Upload a directory with a single copy of each chunk:
```bash
./mbox stash ./my_folder
# [1/2] Unlock Identity
# Enter Master Password: ****
# [2/2] Encrypt Archive
# Enter Archive Password: ****
```
- **Master Password**: To access your email for uploading.
- **Archive Password**: To encrypt the resulting `my_folder.mbox`, empty to reuse the Master Password.

### 3. Put (2× Replication – Redundant Backup)
Upload a directory with double redundancy for safety:
```bash
./mbox put ./my_folder
# Chunks are stored on two different providers automatically.
```

### 4. Get (Restore)
Downloading or sharing is **completely self-contained**. To restore an `.mbox` file you already have, you **do not need any configuration** or a Master Password—only the Archive Password:
```bash
./mbox get my_folder.mbox
# Enter Archive Password: ****
```
*Note: If the file isn't found locally, `mbox` can automatically search your cloud accounts. This is the **only** time it will ask for your local Master Password.*

### 5. Del (Delete)
Remove a local stick file and clean up all remote chunks (wipes data from cloud):
```bash
./mbox del my_folder.mbox
```

### 6. Ls (List)
List all available `.mbox` files stored in your cloud storage (checks the first configured account):
```bash
./mbox ls
# Scanning first mail account for .mbox files...
# MBox Config: project_v1.mbox
# ...
```

### 7. Parallelism (Speed Up)
Speed up uploads and downloads by using multiple concurrent connections across your accounts:
```bash
./mbox put ./large_folder -j 8
# Uploads or downloads multiple chunks in parallel.
```
- **-j, --parallel**: Set the number of concurrent connections (default: 1).
- **-t, --timeout**: Set the timeout for uploading or downloading each data block (default: 5m). Increase this if you have very limited bandwidth (e.g., `-t 10m`).
- **--parallel-by-provider**: If enabled, limits concurrency to only one connection per unique mail provider (host), even if multiple accounts are available for that provider. This helps avoid "Too many connections" errors or throttling from providers like Gmail or Outlook.

---

## 🚀 Design Philosophy
The core philosophy of `mbox` is **Extreme Conciseness**. While traditional distributed filesystems or cloud storage tools require complex mounting, database setups, and persistent credential management, `mbox` simplifies the user experience into a single portable "stick" file (`.mbox`).

- **Portable Brain**: Your metadata and database are packed into a tiny, encrypted `.mbox` file.
- **Zero Infrastructure**: No servers to manage. If you have an email account, you have a storage backend.
- **Self‑Contained**: To share 1 GB of data, you only need to share a tiny `.mbox` file and its password.

---

## 📦 Key Use Cases

### 1. "USB Stick" File Transfer
Forget slow uploads to generic cloud drives or hunting for a physical thumb drive.
- Pack your files into an encrypted `.mbox` stick.
- **Cloud-Enable**: The `.mbox` is automatically stashed in your email.
- **Zero-File Sharing**: Tell the recipient the filename (e.g., `project_v1.mbox`) and password. They run `mbox get project_v1.mbox`, and it pulls everything from the cloud—no need to actually send the file!

### 2. Backup for Medium Folders (< 1 GB)
Ideal for documents, source code repositories, or configuration sets.
- **Redundancy**: `mbox put` automatically implements 2× replication, distributing your data across different email providers.
- **Privacy**: Every byte is encrypted locally with **AES‑GCM** using a password‑derived key (via **Scrypt**) before being sent over SMTP.

---

## 🛠 Command Mapping (to JuiceFS)

`mbox` abstracts the complex lifecycle of a JuiceFS volume into intuitive, one‑shot commands:

| `mbox` Command | Equivalent JuiceFS / System Flow | Purpose |
| :--- | :--- | :--- |
| `mbox config` | Manual config generation | Setup email providers and credentials. |
| `mbox stash` | `format` → `mount` → `cp` → `pack` | 1× replication upload; creates `.mbox` file. |
| `mbox put` | `format` → `mount` → `cp` → `pack` | 2× replication upload; creates `.mbox` file. |
| `mbox get` | `unpack` → `mount` → `cp` | Decrypts `.mbox` and restores data locally. |
| `mbox ls` | `imap search` | Lists .mbox files found in the cloud (first account). |
| `mbox del` | `rm` + `blob cleanup` | Conceptually wipes local and remote data. |

---

## 🌟 Acknowledgement & Co-authorship
`mbox` stands on the shoulders of giants. This project is **co-authored with Gemini 3.0**, whose advanced agentic coding capabilities were instrumental in building and refining this tool.

We also express our deepest gratitude to [JuiceFS](https://github.com/juicedata/juicefs). By leveraging JuiceFS as our metadata and chunking engine, `mbox` inherits industrial‑grade reliability, POSIX‑compatible file handling, and efficient data deduplication while using standard email protocols (IMAP/SMTP) as its "object storage" layer.

---

*Note: `mbox` is optimized for medium‑scale storage. For the best experience, keep individual "sticks" under 1 GB to stay within common email provider daily quotas, OR you can scale up you account list to increase the quota.*