# Saivaree Clinic OpenPost deployment

This directory is the reproducible deployment package for the Saivaree Clinic OpenPost instance.

It intentionally contains **no production secrets**. The real `.env`, SQLite database, media directory, and Cloudflare tunnel credential must be backed up separately.

## Pinned application

- Repository: `https://github.com/siboUNce/openpost.git`
- Application revision: `e031fba1531f4180c840a3a996c9a5ed1a035f05`
- Runtime image tag: `saivaree/openpost:current-main-e031fba`
- Private endpoint: `http://100.67.146.88:4407`
- Public endpoint: `https://post.saivareeclinic.com`

The restore script rebuilds the image from the pinned Git revision. It does not depend on a pre-existing local Docker image.

## Files

- `Dockerfile`: deterministic all-in-one frontend/backend build for the pinned source.
- `docker-compose.yml`: OpenPost + Cloudflare tunnel runtime.
- `.env.example`: safe environment template with placeholders only.
- `restore-openpost.ps1`: rebuild/restore/start/verify workflow.

## Secret and data backups

Keep these outside Git:

- `S:\OpenPost\.env`
- `S:\OpenPost\data\db\openpost.db` or a SQLite `.backup` copy
- `S:\OpenPost\data\media\`
- `C:\ProgramData\Saivaree\cloudflared\config-docker.yml`
- `C:\ProgramData\Saivaree\cloudflared\tunnel.json` (or the original tunnel credential JSON)

Never commit the real `.env`.

## Restore on CLINIC_SERVER

Run PowerShell as Administrator from a checkout containing this directory.

For an existing server where `S:\OpenPost\.env` and data are still present:

```powershell
.\deploy\saivaree-clinic\restore-openpost.ps1
```

For a fresh server with external backups:

```powershell
.\deploy\saivaree-clinic\restore-openpost.ps1 -EnvBackupPath 'D:\Backup\openpost\.env' -DatabaseBackupPath 'D:\Backup\openpost\openpost.db'
```

Restore the media directory and Cloudflare files before running the script.

## Safety behavior

The script:

1. refuses to run on a host other than `Clinic_Server`;
2. requires the original JWT/encryption secrets and never prints them;
3. forces `OPENPOST_DISABLE_TIKTOK_DISPLAY_API=true`;
4. builds exactly the pinned Git revision and checks the image label;
5. validates SQLite integrity and Docker Compose;
6. verifies private/public readiness and exact revision;
7. verifies ChatGPT MCP OAuth still supports public clients (`token_endpoint_auth_methods_supported` contains `none`);
8. verifies the TikTok minimal-scope flag is loaded;
9. creates a rollback checkpoint before replacing an existing runtime.

If there is no production `.env`, the script creates a safe template and stops instead of silently generating new secrets.
