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
mkdir -p ~/gdrive
sudo cp bin/gclone-mount /usr/local/bin/gclone-mount
sudo chmod 755 /usr/local/bin/gclone-mount
sudo cp systemd/gclone.service /etc/systemd/system/gclone.service
sudoedit /etc/systemd/system/gclone.service
sudo systemctl daemon-reload
sudo systemctl enable gclone
sudo systemctl start gclone
```

Set `User`, `Group`, and the mount point in the copied unit to match your machine before starting the service.

Check status and logs:

```bash
systemctl status gclone
tail -f ~/.config/rclone/gclone.log
```

### 8. Optional: Selective Sync

You can make the mount behave more like Dropbox selective sync by showing only chosen folders.

1. Copy the template:

```bash
cp config/gclone-selective-sync.txt.template ~/.config/rclone/gclone-selective-sync.txt
```

2. Edit `~/.config/rclone/gclone-selective-sync.txt` with `rclone` filter rules. Example:

```text
+ /Research/**
+ /Teaching/**
- *
```

3. Restart the service:

```bash
sudo systemctl restart gclone
```

When the filter file is absent or empty, the full remote is mounted. When it contains rules, only matching paths appear in the mount.

### 9. Optional: Selective Sync GUI

If you would rather click folders than edit filter rules, run the local web UI:

```bash
chmod +x bin/gclone-selective-sync-ui
./bin/gclone-selective-sync-ui --mountpoint /path/to/mountpoint
```

Then open `http://127.0.0.1:43123`.

The UI:

- lists top-level folders in the remote
- lets you include an entire top-level folder or only selected subfolders
- writes `~/.config/rclone/gclone-selective-sync.txt`

After saving, apply the new selection with:

```bash
sudo systemctl restart gclone
```

On large Drives, `--mountpoint` is the recommended mode because the GUI can discover folders directly from the mounted filesystem instead of repeatedly querying the Drive API by path.

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
