# Gclone

Gclone *(a modified version of the [rclone](https://github.com/rclone/rclone))* is a command-line program to sync files and directories to and from Google Drive.

## Features

- Synced with rclone version for getting the latest features and bug fixes
- Provides dynamic replacement of the Service Accounts (SAs) for bypassing the 750GB/day limit of Google Drive


## Setup

### 1. Create a Google Cloud Project

1. Go to [console.cloud.google.com](https://console.cloud.google.com)
2. Click the project dropdown at the top → **New Project** → give it a name (e.g. `gclone-sync`) → **Create**

### 2. Enable the Google Drive API

1. In your project, go to **APIs & Services → Library**
2. Search for "Google Drive API" → click it → **Enable**

### 3. Create Service Accounts

You can do this manually via the web UI, or automatically using the provided script (recommended).

**Option A — Automated (gcloud CLI)**

Make sure you're logged in and have a project set:

```bash
gcloud auth login
gcloud config set project YOUR_PROJECT_ID
```

Then run:

```bash
# Create 5 service accounts (adjust the number as needed)
./bin/create_service_accounts.sh 5

# Or specify the project explicitly
./bin/create_service_accounts.sh 5 my-project-id
```

The script creates the accounts, downloads their JSON keys into `accounts/1.json`, `accounts/2.json`, etc., and prints all their emails at the end. It is safe to re-run — existing accounts and keys are skipped.

**Option B — Manual (web UI)**

1. Go to **APIs & Services → Credentials**
2. Click **+ Create Credentials → Service Account**
3. Give it a name (e.g. `sa1`) → **Create and Continue** → skip role → **Done**
4. Click the service account → **Keys** tab → **Add Key → Create new key → JSON** → download the file
5. Repeat to create multiple service accounts (e.g. 5–10) — more accounts means more rotation and higher throughput

### 4. Place the JSON Key Files (manual setup only)

```bash
mkdir -p ~/gclone/accounts
# Move downloaded JSON files there, renamed 1.json, 2.json, etc.
mv ~/Downloads/*.json ~/gclone/accounts/
```

### 5. Share Your Google Drive with the Service Accounts

Each service account has an email like `sa1@your-project.iam.gserviceaccount.com`. Print them all with:

```bash
for f in ~/gclone/accounts/*.json; do
    python3 -c "import json; d=json.load(open('$f')); print(d['client_email'])"
done
```

Then in Google Drive, share your root folder (or Shared Drive) with each email, granting **Editor** access.

### 6. Configure rclone

Create `~/.config/rclone/rclone.conf` with:

```ini
[gc]
type = drive
scope = drive
service_account_file = /path/to/gclone/accounts/1.json
service_account_file_path = /path/to/gclone/accounts/
root_folder_id = root
```

### 7. Install the systemd Service

```bash
sudo cp bin/gclone-mount /usr/local/bin/gclone-mount
sudo chmod 755 /usr/local/bin/gclone-mount
sudo cp systemd/gclone.service /etc/systemd/system/gclone.service
```

Open the copied unit and set `User`, `Group`, the mount point, and your config/log paths:

```bash
sudoedit /etc/systemd/system/gclone.service
```

The service template includes two lines that wire up selective sync — keep them:

```ini
# Ensures the filter file exists before gclone starts (empty = show everything)
ExecStartPre=/bin/bash -c 'mkdir -p %h/.config/rclone && touch -a %h/.config/rclone/gclone-selective-sync.txt'
# Passes the filter file to gclone on every start
ExecStart=... --filter-from=%h/.config/rclone/gclone-selective-sync.txt ...
```

Enable and start:

```bash
sudo systemctl daemon-reload
sudo systemctl enable gclone
sudo systemctl start gclone
```

Check status and logs:

```bash
systemctl status gclone
tail -f ~/.config/rclone/gclone.log
```

### 8. Optional: Auto-restart after Suspend / Hibernate

When the system resumes from sleep the FUSE mount goes stale. A systemd-sleep hook restarts it automatically:

```bash
sudo mkdir -p /etc/systemd/system-sleep
sudo cp systemd/gclone-resume /etc/systemd/system-sleep/gclone-resume
sudo chmod +x /etc/systemd/system-sleep/gclone-resume
```

No daemon-reload is needed — systemd-sleep hooks are plain shell scripts. On resume, systemd calls the script with `post <sleep-type>`, which triggers `systemctl restart gclone`.

### 9. Optional: Selective Sync

The mount can be configured to show only chosen folders, exactly like Dropbox selective sync. The filter is an rclone filter file at:

```
~/.config/rclone/gclone-selective-sync.txt
```

When the file is **empty or absent** the full Drive is mounted. When it contains rules, only matching paths appear. Files sitting loose at the Drive root (not inside any folder) are always included automatically via a `+ /*` rule.

Example filter file for two folders plus loose root files:

```text
# Managed by selective-sync-gui
+ /Research/**
+ /Teaching/**
+ /Teaching/
+ /Teaching/2024/**
+ /*
- *
```

Edit the file directly, or use the GUI (recommended — see below). After any change:

```bash
sudo systemctl restart gclone
```

### 10. Optional: Selective Sync GUI

A browser-based UI lets you choose folders with checkboxes instead of editing filter rules by hand.

**Start the UI**

```bash
chmod +x bin/gclone-selective-sync-ui
./bin/gclone-selective-sync-ui --mountpoint /path/to/mountpoint
```

Then open `http://127.0.0.1:43123`.

Pass `--mountpoint` pointing at the already-mounted Drive path (recommended). This lets the UI discover folders from the local filesystem rather than querying the Drive API over the network.

**Interface**

| Element | What it does |
|---|---|
| **Select all folders** checkbox | Checks or clears every top-level folder at once |
| **▶ / ▼** arrow | Expands a folder to reveal its subfolders |
| **Folder checkbox** | Includes the entire folder and all its contents |
| **Subfolder checkboxes** | Includes only those specific subdirectories (folder checkbox must be unchecked) |
| **all / none** links | Bulk-select or clear subfolders inside an expanded folder |
| **Search box** | Filters the visible folder list by name |
| **Save Selection** | Writes `~/.config/rclone/gclone-selective-sync.txt` |
| **Restart gclone** | Runs `sudo systemctl restart gclone` and shows the result in the terminal |
| **Show Entire Drive** | Clears the filter file so all folders are visible |
| **Reload** | Re-reads the filter file and Drive folder list |

**Status pill**

A small indicator above the folder list shows the live service state, refreshing every 4 seconds:

| Colour | Meaning |
|---|---|
| Green | Running, up to date |
| Blue (pulsing) | Downloading or uploading files |
| Red | Service stopped or failed |

**Command-line status**

```bash
# Quick one-liner
systemctl status gclone --no-pager -l | grep -E "Active:|Status:"

# Live log stream
tail -f ~/.config/rclone/gclone.log
```

**Flags**

```
--addr            Listen address (default 127.0.0.1:43123)
--mountpoint      Path to the mounted Drive (strongly recommended)
--remote          rclone remote name (default gc:)
--config          rclone config file (default ~/.config/rclone/rclone.conf)
--filter-file     Filter file path (default ~/.config/rclone/gclone-selective-sync.txt)
--restart-command Command run by the Restart button (default: sudo systemctl restart gclone)
--gclone-bin      Path to the gclone binary (default /usr/local/bin/gclone)
```

## Instructions

### 1. Configuring the service_account_file_path

Add `service_account_file_path` in config file for dynamic replacement of Service Accounts (SAs). Replaces when `rateLimitExceeded` error occurs.

> ***rclone.conf*** example:
```
[gc]
type = drive  
scope = drive  
service_account_file = /root/accounts/1.json  
service_account_file_path = /root/accounts/  
root_folder_id = root  
```
**Note:** `/root/accounts/` folder must contain **SA files** (*.json)
  
### 2. Copying data

```
gclone copy gc:{source} gc:{destination} --drive-server-side-across-configs
```
**Note:** Provide Team Drive ID or Folder ID as `source` and `destination`

## Caveats

Creating Service Accounts (SAs) allows you to bypass some of Google's quotas. Tools like Autorclone and gclone automatically rotates SAs for continuous multi-terabyte file transfer.

> Quotas SAs **CAN** bypass:
* Google 'copy/upload' quota (750GB/account/day)
* Google 'download' quota (10TB/account/day)

> Quotas SAs **CANNOT** bypass:
* Google 'Shared Drive' quota (~20TB/drive/day)
* Google 'file owner' quota (~2TB/day)

## Credits

- [rclone](https://github.com/rclone)
- [donwa](https://github.com/donwa)
- [dogbutcat](https://github.com/dogbutcat)
