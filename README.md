# Backup Manager

Backups for databases and folders on your own servers, with a web panel. One program, installed with one command.

- **MySQL / MariaDB** — SQL backups of the whole server, chosen databases or chosen tables, taken from one consistent
  moment without locking the application; **hot physical backups** of MariaDB with incrementals; **binary-log
  archiving** and restore to any second.
- **PostgreSQL**, **Redis** and **folders**.
- Runs on a schedule, keeps what you tell it to keep, deletes the rest, and **proves** backups by restoring them.
- Encrypted (AES-256) and compressed before they leave the server; stored on the same server, any S3-compatible
  storage (AWS, Cloudflare R2, Backblaze B2, Wasabi, MinIO…), or both.
- Look inside any backup and **search its rows** in the browser; download all of it or one table; restore one table
  under a new name.
- Accounts with roles, two-factor sign-in, API tokens, command line, Prometheus metrics, Telegram / webhook alerts.
- **Agents**: one panel backs up many servers.
- English and Arabic panel.

## Install

On a fresh Linux server (Debian 11+, Ubuntu 20.04+, RHEL / Rocky / Alma 8+; x86-64 or ARM64; systemd), as root:

**If this repository is public**

```bash
curl -fsSL https://raw.githubusercontent.com/SamyJoe-1/Backup-Manager-v2/main/install.sh | sudo bash
```

**If this repository is private** (you need a GitHub token that can read it, see
[docs/INSTALL.md](docs/INSTALL.md#private-repository))

```bash
export GITHUB_TOKEN=github_pat_xxxxxxxx
curl -fsSL -H "Authorization: Bearer $GITHUB_TOKEN" \
  https://raw.githubusercontent.com/SamyJoe-1/Backup-Manager-v2/main/install.sh | sudo -E bash
```

**From a copy of the repository**

```bash
git clone https://github.com/SamyJoe-1/Backup-Manager-v2.git
cd Backup-Manager-v2
sudo ./install.sh
```

The installer asks a few questions (how you will open the panel, which account it runs as, Docker access…), installs
everything, starts the service and prints **a one-time link to create your account**. Open it, then follow
[docs/USER-GUIDE.md](docs/USER-GUIDE.md).

**Keep a copy of `/etc/backup-manager/master.key` somewhere else** (a password manager). Without it no encrypted
backup can ever be read.

## Documentation

| Read | When |
|---|---|
| [docs/INSTALL.md](docs/INSTALL.md) | installing: every option, HTTPS, Docker, agents, container, upgrade, uninstall, troubleshooting |
| [docs/USER-GUIDE.md](docs/USER-GUIDE.md) | after installing: databases, destinations, plans, schedules, alerts, users, everything in the panel |
| [docs/RESTORE.md](docs/RESTORE.md) | getting data back: every kind of backup, point in time, and when the backup server itself is lost |
| [docs/HOW-IT-WORKS.md](docs/HOW-IT-WORKS.md) | why a backup at peak time does not hurt production; storage format, encryption, queue |
| [docs/API.md](docs/API.md) | API, tokens, command line, metrics |
| [docs/DEVELOPMENT.md](docs/DEVELOPMENT.md) | building, tests, releases |

## Known limits

Tested on small data (tens of MB) in throwaway containers; not yet measured on a database of tens of GB.
Physical backups are MariaDB only (not MySQL). PostgreSQL has no point-in-time restore. Redis is snapshot only.
Restoring a physical backup stops at "data folder ready + exact commands"; switching your server over to it is done by
hand, on purpose. Details: [docs/HOW-IT-WORKS.md](docs/HOW-IT-WORKS.md#limits).
