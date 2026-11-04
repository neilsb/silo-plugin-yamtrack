# Yambridge — Silo → Yamtrack

A native Silo watch-provider plugin for **new completed movie and episode watches**.
It sends Jellyfin-compatible webhook messages to Yamtrack. **v0.2.0** adds
immediate completed-stop delivery and optional episode ID lookup. No extra
running container is needed: the plugin runs inside Silo.

## Install from the Silo repository

In **Admin → Integrations → Plugins → Repositories**, add:

```text
https://raw.githubusercontent.com/neilsb/silo-plugin-yamtrack/main/catalog.json
```

Refresh the catalog and look for **Yambridge** under **Watch Providers**. This
is an external repository maintained by `neilsb`. Release downloads and checksums
are hosted at [GitHub Releases](https://github.com/neilsb/silo-plugin-yamtrack/releases).
Follow the Docker/state-volume and profile connection instructions below.

For an existing file-upload installation, retain `community.yambridge`, the
`yamtrack` capability, the profile connection, and the persistent volume. Check
the installation's source after adding the catalog; simply adding a repository
does not establish that an uploaded installation has become catalog-managed.
Use Silo's catalog install/update flow to associate it if supported by your
server version. Do not uninstall it just to make the catalog entry appear.

Built against Silo plugin SDK **v0.32.1**, and checked against the source of
Yamtrack **v0.26.3**. The installed Silo server's scheduled-export adapter was
also inspected while debugging: it emits `MARK_WATCHED` with `RECONCILIATION`
origin and leaves the playback-only `completed` flag unset. Version **0.1.3**
handles this contract. The host must support `watch_sync_provider.v1` and
API-key connections. Watch providers are a Silo beta feature. Automated tests
use local HTTP servers. Live movie delivery was verified on November 3, 2026:
Silo scheduled export → HTTP 200 → Yamtrack created a Completed movie entry.
The new live-stop path and optional lookup have automated integration tests;
live end-to-end verification of those features remains to be done.

## Upgrading to v0.2.0

Upload the new binary and keep the existing connection and state volume.
In **Settings → Watch Providers → Yamtrack**, enable **Scrobble playback** as
well as **Send watched changes**. No reconnect or extra credential is needed
for immediate delivery or for episodes that already have TVDB/IMDb IDs.

**TMDB lookup is optional and off without a token.** If you want the fallback,
open Yambridge's plugin settings under **Admin → Integrations → Plugins** and
enter a **TMDB API Read Access Token** from your TMDB account's API settings.
This is the bearer token, not the shorter v3 API key. Existing profile connections
use these server-wide lookup settings without reconnecting.

## Build

With Docker BuildKit, export the binary for the architecture of your Silo server:

```sh
docker build --build-arg TARGETARCH=amd64 --output type=local,dest=dist .
```

Use `TARGETARCH=arm64` for an ARM64 server. Upload `dist/yambridge` to Silo.
This Dockerfile builds a plugin binary, not a standalone service image.

Alternatively, with Go 1.26 or newer:

```sh
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o dist/yambridge .
./dist/yambridge manifest
```

## Docker and installation

1. Add a persistent writable mount to your **existing Silo service**. Merge these
   entries into your Compose file (replace `silo` with your service name):

   ```yaml
   services:
     silo:
       volumes:
         - yambridge-state:/var/lib/yambridge
   volumes:
     yambridge-state:
   ```

   The directory must be writable by Silo's container user. For a non-root
   container, provision the volume with that user's UID/GID. The connection test
   reports an error if it cannot open the database. An optional
   `YAMBRIDGE_STATE_DIR` environment variable changes the location if the host
   passes it to plugin processes. Keep this mount on local storage, not NFS.

2. Recreate the Silo container with the updated Compose configuration.
3. In Silo, open **Admin → Integrations → Plugins**, use **Manual Install**, and
   upload the binary matching your server architecture.
4. In your Yamtrack account's settings, find the **Jellyfin integration** and copy
   its webhook URL. It has a path ending in `/webhook/jellyfin/<token>`.
5. Make sure that URL is reachable **from the Silo container**. On a shared Docker
   network you can use Yamtrack's service name and internal port, for example
   `http://yamtrack:8000/integrations/webhook/jellyfin/<token>` (retain the actual
   path from your copied URL). `localhost` inside Silo means the Silo container.
   Configure Yamtrack's allowed hosts for the hostname you use, if required.
6. Switch to the Silo profile you want to track. Open **Settings → Watch Providers**,
   connect **Yamtrack (Yambridge)**, and paste the **entire webhook URL** into the
   API key field. The URL contains the credential; Silo stores it encrypted.
7. Enable **Send watched changes** and **Scrobble playback** for this connection.
   Repeat with each profile's corresponding Yamtrack account.

With scrobbling enabled, Yambridge sends a webhook when Silo reports a **completed
playback stop**, without waiting for a scheduled sync. Completion is determined
by Silo's watched threshold, not by Yambridge guessing a percentage. Starts,
pauses, and incomplete stops do not create Yamtrack entries. Reaching the watched
threshold alone does not send a webhook until Silo emits the completed stop.

Scheduled watched export remains available as a fallback, and works on its own
if scrobbling is disabled. The inspected installation runs **sync_watch_providers**
hourly. Silo's **Sync now** button has a cooldown; the administrator's Scheduled
Tasks page also offers **Run Now**. Live and scheduled events share a persistent
watch-history record so the same watch is not posted twice.

The connection check sends an ignored event that validates the Yamtrack token
without recording a watch. It establishes a start time: watched events dated
before that time are ignored, including during scheduled reconciliation.
`MARK_WATCHED` is the host's watched decision; `SCROBBLE_STOP` also requires
the host's `completed=true` flag. Direct manual-mark events are ignored, but a new manual mark can
later be included in scheduled exports: those events do not preserve the
original playback-versus-manual origin.
The plugin advertises watched export and playback scrobbling. It does not
import history, export unwatched changes, or send in-progress/resume state.

## Matching and behavior

| Media | Required identifiers in Silo |
| --- | --- |
| Movie | Movie TMDB or IMDb ID |
| Episode (default) | **Episode-level** TVDB or IMDb ID |
| Episode (optional TMDB fallback) | Show TMDB, IMDb, or TVDB ID, plus season and episode numbers |

Yambridge uses Silo's own episode IDs first and makes no metadata request when
they are present. Since Yamtrack v0.26.3's webhook needs an episode-level TVDB or
IMDb ID, the optional fallback resolves those IDs through TMDB:

1. Use the show's TMDB ID, or resolve its IMDb/TVDB ID to one unambiguous TMDB show.
2. Read that show's episode external IDs by season and episode number.
3. Send the resolved **episode** TVDB/IMDb IDs to Yamtrack.

The token is supplied by Silo in each authenticated plugin call, sent only to
TMDB, and never written to the delivery database or diagnostics. With the token
blank, there are no TMDB requests. Missing episode IDs then produce an explicit
error, as before. The plugin does not search by title or substitute show IDs for
episode IDs. Specials (season zero) are supported. Fallback numbering must match
TMDB; if Silo also provides a TMDB episode ID, it must agree with the lookup.
Anime handling ultimately follows Yamtrack's account preferences and mappings.

A completed watch is sent as `Event: Stop` with `Item.UserData.Played: true`.
Yamtrack owns metadata resolution and tracking updates. A successful HTTP response
means Yamtrack accepted the webhook, **not** proof that it found a matching title:
its endpoint also returns 200 when metadata cannot be matched. Inspect Yamtrack
logs if a watch does not appear. Dates are assigned by Yamtrack when it processes
the webhook; original timestamps and resume positions are not imported. Rewatches
use distinct Silo watch-history IDs and follow Yamtrack's existing rewatch behavior.

## Delivery and restarts

Yamtrack's webhook has no idempotency key, and repeated movie events can create
duplicate watches. A persistent bbolt database records hashes of the destination,
event ID, and shared watch-history ID before sending, then records successful
delivery. The shared ID links live completion with scheduled export even if
their event IDs and timestamps differ. It contains no
webhook URLs, tokens, titles, or payloads. Keep it across plugin/container updates
and back it up along with Silo. Do not delete it or run copies against separate
databases for the same connection; doing so loses duplicate protection.

- Already delivered watch: ignored across live/scheduled paths and restarts.
- Pre-upgrade event-ID records are preserved and checked by the live path.
- Completed live stop without a watch-history ID: rejected rather than claiming
  success, leaving scheduled export available. Session IDs or approximate
  timestamps alone are not sufficient to safely correlate those two paths.
- Database unavailable before sending: Silo can retry.
- Temporary TMDB failure or rate limit: retryable; no webhook has been sent and
  no uncertain-delivery marker is recorded. Already delivered watches skip lookup.
- HTTP 401/403: connection error; HTTP 429: deferred retry.
- Timeout, connection loss, server error, redirect, or a crash during delivery:
  **uncertain**, and not automatically resent. The watch may or may not have been
  saved. Check Yamtrack and add it manually if missing. This trades automatic
  recovery for avoiding duplicate watch records; it is not exactly-once delivery.

The ledger grows with delivered events and is intentionally retained. Reconnecting
sets a new start time. Changing the destination URL/token changes its ledger
namespace. A plugin connection represents a profile; normally connect different
Silo profiles to different Yamtrack accounts.

## Verify after installation

1. Connect successfully and confirm existing history is not imported.
2. Complete a movie with a TMDB ID and check its entry in Yamtrack.
3. Complete an episode with an episode-level TVDB/IMDb ID and check the episode.
4. Stop another title early and confirm it is not recorded as completed.
5. After a live completion is sent, run the scheduled sync and confirm no duplicate.
6. If using the optional token, test an episode with show IDs but no episode-level
   TVDB/IMDb ID. Diagnostics should show `episode_ids_resolved` before delivery.
7. Check the Silo provider card and Yamtrack logs for failures.

## Debugging (v0.1.2+)

Diagnostic events are written to stderr and to
`/var/lib/yambridge/diagnostics.log` inside the Silo container (or the configured
state directory). The file remains available even if Silo filters plugin stderr.
It rotates at 1 MiB, keeping one previous file as `diagnostics.log.1`.
Logs contain event references, operation/origin/completion flags, skip reasons,
and HTTP status codes, but no webhook URLs, tokens, titles, or request bodies.

Replace `SILO_CONTAINER` with the actual container name:

```sh
docker exec SILO_CONTAINER cat /var/lib/yambridge/diagnostics.log
```

After updating the plugin, retain the existing connection and state volume.
Check **Settings → Watch Providers → Yamtrack** for **Send watched changes**,
**Scrobble playback**, errors, and export status. Complete a new playback and
stop it; if testing only scheduled delivery, run the sync task afterward.

- `started` but no `apply_events`: no export RPC has reached the plugin; inspect
  Silo's selected profile, provider connection, export setting, and sync status.
- `event_skipped`: the `reason` identifies the filter. Include the preceding
  `event_received` record when reporting this. Versions before 0.1.3 incorrectly
  skipped scheduled exports because of their origin and missing completion flag.
- `event_rejected`: inspect the identifier or timestamp error.
- `event_result` with `REJECTED` or `RETRY`: the reason describes a metadata or
  delivery problem. A rejected TMDB configuration/match may need a fresh watch
  or targeted retry after correcting it; Silo controls export retry policy.
- `metadata_lookup_result` / `episode_ids_resolved`: optional fallback activity.
- `webhook_result` with HTTP 200: Yamtrack accepted the request; investigate
  its metadata matching if the entry is missing.
- `transport_error: true` or a non-200 status: investigate networking or the
  Yamtrack endpoint. An uncertain request is not automatically resent.

The new logging cannot reconstruct events already skipped by an earlier version.
Silo counts `NO_CHANGE` as sent, so upgrading alone does not retry those records.
Use a new playback completion to test, or arrange a targeted recovery of a
confirmed skipped export. Keep the connection and database when upgrading so
the original cutoff and duplicate protection remain intact.

Development checks:

```sh
go test -race ./...
go vet ./...
```

## Publishing a release

The `Release` GitHub Actions workflow runs when a stable version tag is pushed.
It tests the code, cross-compiles Linux AMD64/ARM64 binaries, generates SHA-256
checksums and a Silo catalog, publishes the assets, then updates `catalog.json`
on `main`. The catalog is updated only after the release is published. It uses
the workflow's built-in GitHub token with `contents: write`; no custom token or
Silo-maintainer approval is needed for this external repository.

1. Update the version in **both** `manifest.json` and `diagnostics.go`.
2. Commit and push the source to `main`.
3. Tag that commit and push the tag, for example:

   ```sh
   git tag v0.2.1
   git push origin v0.2.1
   ```

4. Verify the workflow succeeds and the catalog points at the published assets.

Tags must match the manifest version. Published binaries are not overwritten:
a workflow rerun checks existing checksums before repairing a missing catalog
update. Use a new version for changed code. If branch protection is added to
`main`, allow the catalog-update workflow to push or adapt it to open a PR.

To generate release metadata locally after building `dist/plugin-linux-amd64`
and `dist/plugin-linux-arm64`:

```sh
go run ./cmd/catalog -tag v0.2.0 -dist dist
```

This writes `dist/checksums.txt` and `dist/catalog.json`. The repository index
wraps the plugin manifest in Silo's `plugins` structure; the root `manifest.json`
alone is not a repository index. The generator uses Silo's catalog JSON encoding
(numeric protobuf enums) and checks the publisher/source metadata.

References: [Silo watch providers](https://siloserver.org/docs/watch-sync),
[Silo SDK](https://github.com/Silo-Server/silo-plugin-sdk),
[Yamtrack v0.26.3 webhook code](https://github.com/FuzzyGrim/Yamtrack/tree/v0.26.3/src/integrations/webhooks).
