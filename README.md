# Restop

Restop is a small, read-only web browser for one [restic](https://restic.net/) repository. It lists snapshots, browses their directory trees, and streams files or directories for download. Directory downloads are uncompressed tar archives produced directly by `restic dump`.

Restop does not include authentication. It binds to loopback by default; do not expose it to a network without an authenticating reverse proxy and transport encryption. Anyone who can reach the service can inspect and download every file in the configured repository.

## Requirements and running

- Go 1.26 or newer to build
- A separately installed `restic` executable
- A configured restic repository and non-interactive credentials

```sh
go build -o restop ./cmd/restop
RESTIC_REPOSITORY=/srv/backups \
RESTIC_PASSWORD_FILE=/run/secrets/restic-password \
./restop
```

Open <http://127.0.0.1:8080>. Restop performs a startup preflight and exits if the executable, repository, or credentials do not work. It never accepts credentials over HTTP and never invokes a shell for repository operations.

Restic's standard environment is inherited, including `RESTIC_REPOSITORY` or `RESTIC_REPOSITORY_FILE`, `RESTIC_PASSWORD_FILE` or `RESTIC_PASSWORD_COMMAND`, `RESTIC_CACHE_DIR`, and backend-specific credentials. `RESTIC_PASSWORD` also works, but a permission-restricted password file or secret manager command is safer because process environments can leak through diagnostics and process inspection. Run Restop as a dedicated, unprivileged account and limit that account's access to only the required secrets.

## Application configuration

| Variable                  | Default          | Purpose                                                       |
| ------------------------- | ---------------- | ------------------------------------------------------------- |
| `RESTOP_ADDR`             | `127.0.0.1:8080` | HTTP listen address                                           |
| `RESTOP_RESTIC_PATH`      | `restic`         | Restic executable name or path                                |
| `RESTOP_METADATA_TIMEOUT` | `1m`             | Timeout for snapshot and directory metadata commands          |
| `RESTOP_MAX_COMMANDS`     | `8`              | Maximum total concurrent restic processes                     |
| `RESTOP_MAX_DOWNLOADS`    | `2`              | Maximum concurrent downloads; cannot exceed the command limit |
| `RESTOP_SHUTDOWN_TIMEOUT` | `30s`            | Time allowed for active requests before forced cancellation   |
| `RESTOP_LOG_LEVEL`        | `info`           | JSON log level: `debug`, `info`, `warn`, or `error`           |

The liveness endpoint is `GET /healthz`. It only reports that the process is serving HTTP and never returns repository information. Repository failures do not make liveness fail.

## Reverse proxy deployment

Keep Restop on a loopback or private Unix-network boundary and put an authenticating proxy in front of it. The proxy should:

- require authentication and HTTPS;
- preserve streaming responses without buffering large downloads;
- apply appropriate request and idle timeouts for long downloads;
- restrict access to trusted users and networks;
- avoid logging download query strings if repository paths are sensitive.

Restop intentionally has no mutation endpoints, database, metadata cache, search, preview, or multi-repository mode. The embedded HTMX asset enhances navigation, but every browsing link and download continues to work with JavaScript disabled.

HTMX 2.0.8 is pinned and vendored into the executable; its license is recorded in [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).
