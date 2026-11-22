# AGENTS.md

Guide for anyone, human or AI agent, maintaining ElVa. User-facing documentation is in
[README.md](README.md).

A maintainer's local, private notes (machines, credentials, deployment specifics) go in
`AGENTS_OWNER.md`, which is git-ignored. If it exists, read it too. Never commit it, and
keep anything specific to one installation out of this file.

## Rules

1. **Keep the docs current.** Every change that affects behaviour, configuration,
   output, the RCU protocol or the roadmap must update `README.md` (for users) and
   this file (for maintainers) in the same change.
2. **The RCU is shared, so ElVa must stay read-only and unobtrusive.** The utility
   company may read meters through the same device for billing, and people may be
   using its web UI at the same time.
   - Only call `POST /index.htm` (login) and `POST /mb.xml` with `pos=N` or `counter=1/2`.
     Never POST to `mb.htm`, `ss_*.htm` or any other `.htm` form: those save settings.
     Never send `counter=0`: it resets the shared cursor to MB01.
   - Always restore the shared M-Bus cursor after a scrape (`scrape()` in `cmd/elva/main.go`).
   - Read at most once a minute, with requests sent one at a time. The RCU is a small
     embedded device and closes the connection after every response.
3. **Never commit secrets or private notes.** Git ignores `.env.*` (except `.env.sample`),
   `AGENTS_OWNER.md` and `CLAUDE.local.md`. Keep local settings in a personal file such
   as `.env.<name>`. Tests use the fake password `secret`, and test fixtures must not
   contain real names, serial numbers or addresses.
4. **Use the standard library only, unless a dependency clearly pays for itself.** The
   only direct dependencies are `github.com/jackc/pgx/v5` (Go has no built-in
   PostgreSQL driver) and `github.com/modelcontextprotocol/go-sdk` (the official MCP
   SDK).
5. **Define every setting in its binary's `settings` table.** That is
   `cmd/elva/config.go` for the scraper and `cmd/elva-mcp-stdio/main.go` for the MCP
   server; both use `internal/config`. Don't call `os.Getenv` anywhere else. The table
   drives the command-line flags, `-help` and the startup settings log, and secrets
   must not leak into logs.
   - Mark a setting `Secret: true` if it holds a credential; names containing
     PASSWORD, SECRET or TOKEN and passwords inside URLs are masked anyway.
   - Every setting automatically gets a flag (`config.FlagName`: `ELVA_` dropped,
     lower case, dashes). Pick names whose flag reads well and doesn't clash with
     `-check`, `-version`, `-env-file` or `-help`.
   - Also add every new setting to `.env.sample` (scraper) or `deploy/mcp.env.sample`
     (MCP server), and to README.md's settings tables, with its flag.
6. **The MCP server must stay read-only and bounded.** Keep every safety layer:
   - the session is read-only (`default_transaction_read_only`), and `query` also runs
     in a read-only transaction;
   - the statement timeout and the row limit;
   - single statements only (pgx's extended protocol);
   - a `SELECT`-only role (`deploy/readonly-role.sql`).

   Tool output goes into a model's context, so keep answers small: cap the number of
   buckets and points, and group long ranges, as `get_baseload` does.
7. **Keep tool schemas portable.** Some MCP clients (and model providers with
   single-type schema dialects) refuse `"type": [...]` arrays and bare `true`/`false`
   schemas.
   - `add()` builds every schema with `schemaFor` (`cmd/elva-mcp-stdio/schema.go`),
     which rewrites nullable types to `anyOf`.
   - A Go `any` in a result needs a hand-written schema (see `queryOutputSchema`).
   - `TestTools` fails on any unportable construct.

## Build, test, run

Requires Go 1.25 or newer (pgx needs it). If Go was installed from the tarball into a
user directory such as `~/.local/go`, put it on the PATH, e.g. in `~/.bashrc`:
`export PATH="$HOME/.local/go/bin:$HOME/go/bin:$PATH"`. Otherwise you get
`go: command not found`.

```sh
go vet ./... && go test ./...
scripts/build.sh                                      # bin/elva and bin/elva-mcp-stdio, with version
./bin/elva -env-file .env.<name>                      # real RCU
ELVA_INTERVAL=10s ./bin/elva -env-file .env.<name>    # faster, for manual testing only
```

To check the startup failure paths:
- an empty env gives exit 1;
- `KTC_HOST` set to an unused IP gives exit 2;
- a wrong `KTC_PASSWORD` gives exit 3;
- an unreachable `ELVA_DATABASE_URL` gives exit 5.

### Database tests

Tests in `internal/store` and `cmd/elva-mcp-stdio` run against a real PostgreSQL when
`ELVA_TEST_DATABASE_URL` is set, and are skipped otherwise.
- `internal/testdb` gives each test package its own schema (`?search_path=...`), so
  packages running in parallel don't collide.
- **It drops and recreates that schema**, so use a throwaway database only.
- The read tests use a synthetic dataset covering a DST change, a gap and a night
  "leak".

```sh
docker run -d --rm --name elva-test-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=elva \
  -p 127.0.0.1:55432:5432 postgres:17
ELVA_TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:55432/elva go test ./...
docker stop elva-test-pg    # --rm deletes it
```

To test an outage end to end, run ElVa against that container with
`ELVA_INTERVAL=10s`. Then `docker pause elva-test-pg` for a few scrapes, followed by
`docker unpause`. The log should show "buffering" warnings, then "buffered rows
written", and the table should have no gap.

### Configuration loading

- **Priority:** flag, then environment, then `-env-file`, then default.
  `config.Resolve` implements it and the startup log shows each value's source.
- Settings are mainly read from the environment, which follows the 12-factor
  convention.
- **Flags** are a convenience for containers and MCP client configs. An empty flag
  counts as not given. A secret from a flag works but logs a warning, because other
  local users can see a process's arguments.
- `-env-file` is an opt-in convenience for running by hand: nothing is loaded
  automatically. Values already in the environment win over the file, so a
  deployment can override a single value.
- In production, prefer the supervisor's own mechanism, e.g. systemd
  `EnvironmentFile=` or compose `env_file:`.
- The file parser is deliberately minimal: `KEY=VALUE`, `#` comments, an optional
  `export ` prefix and surrounding quotes. It has no multi-line values or escape
  sequences.

### Logging

- Logs go to the console only (`log/slog` text to stderr), with no log-file option
  in ElVa.
- The supervisor stores and rotates them: systemd's journal (`journalctl -u elva`) or
  `docker logs`.
- For a plain file, redirect: `2>> elva.log`, or systemd
  `StandardError=append:/var/log/elva.log`.
- `elva`: normal scrapes log nothing; only startup, warnings and errors are logged.
- `elva-mcp-stdio`: startup, then one line per tool call (name, duration, error). Its
  stdout is the MCP protocol, so it must never print anything else there.

## Layout

| Path | Contents |
|------|----------|
| `cmd/elva/main.go` | Startup checks (database, RCU), minute loop, cursor restore, meter freshness check (`meterAgeProblem`), database write or JSON output |
| `cmd/elva/row.go` | Maps readings to columns by quantity (`toRow`), with unit conversion; rejects two meters filling one column |
| `cmd/elva/buffer.go` | In-memory row buffer for database outages (`ELVA_DATABASE_BUFFER`), flushed oldest first in chunks |
| `cmd/elva/config.go` | The scraper's settings table and `parseConfig` |
| `cmd/elva-mcp-stdio/main.go` | MCP server: settings, read-only database connection, `-check`, stdio transport |
| `cmd/elva-mcp-stdio/tools.go` | Tool definitions and handlers, server instructions, `get_schema` |
| `cmd/elva-mcp-stdio/schema.go` | `schemaFor`: portable JSON Schemas for tool inputs and outputs (rule 7) |
| `cmd/elva-mcp-stdio/when.go` | Parses time inputs (`2026-11`, `-7d`, RFC 3339, ...) in local time |
| `internal/config/config.go` | Shared settings machinery: flags (`Flags`, `FlagName`), env file, priority and sources, secret masking, `-help` |
| `internal/ktc/client.go` | HTTP client: `Login`, `ReadMeter`, `Cursor`, `SetCursor`; error kinds `ErrUnreachable`, `ErrAuth` |
| `internal/ktc/parse.go` | Start page and `mb.xml` parsing, VIF unit table |
| `internal/ktc/testdata/` | Anonymized responses captured from an RCU1111-1100 (firmware 4.7.13) |
| `internal/ktc/ktc_test.go` | Parser tests and a fake RCU (`newFakeRCU`) that models the session and cursor |
| `internal/store/store.go` | PostgreSQL writer: `Open` (connect, create schema), `Insert` (one `unnest` statement, `ON CONFLICT DO NOTHING`), `Latest`, `Schema` |
| `internal/store/read.go` | Read-only `Reader` for the MCP server: `Consumption`, `Timeseries`, `Baseload`, `Info`, `Query` |
| `internal/testdb/testdb.go` | Test helper: a fresh schema per test package in the test PostgreSQL |
| `scripts/mcp-inspect.sh` | Dev helper: opens the MCP Inspector (Node.js 22+) on `bin/elva-mcp-stdio`, connected to the test database |
| `scripts/build.sh` | Builds `bin/elva` and `bin/elva-mcp-stdio` with `-X main.version=$(git describe --tags --always --dirty)` |
| `deploy/elva.service` | systemd unit template (`@PREFIX@` is replaced by `install.sh`) |
| `deploy/install.sh` | Install or upgrade: user `elva`, `PREFIX/bin/elva`, `PREFIX/elva.env` (never overwritten), unit; enable and restart. Also `elva-mcp-stdio`, group `elva-mcp` and `PREFIX/mcp.env` (0640 root:elva-mcp) |
| `deploy/verify.sh` | Checks files, permissions, settings, unit drift, service state, then `elva -check`, last-row age, the MCP server (`-check`, group members) and journal warnings |
| `deploy/mcp.env.sample` | Template for `PREFIX/mcp.env` |
| `deploy/readonly-role.sql` | Creates the `SELECT`-only role `elva_ro` for the MCP server |
| `deploy/uninstall.sh` | Removes the service and binaries, keeps `elva.env` and `mcp.env`; `--purge` also removes `PREFIX`, the user `elva` and the group `elva-mcp`. Never touches the database |
| `.env.sample` | Template for the reader's settings, in the same order as its `settings` table |
| `CLAUDE.md` | Makes Claude Code read this file (`CLAUDE.local.md`, git-ignored, can add `AGENTS_OWNER.md`) |

## How the RCU web UI works

Tested on RCU1111-1100, firmware 4.7.13. Reference: [KTC user manual part 2](https://docs.ktc.se/media/duc-manualer/Anvandarmanual%20del%202,%20KTC-RCU1111,%20SW-4.3.00,%20T-V%202013-08-13.%20Peo%20Sandberg-Sanna%20Fager%20KTC%20TECH.pdf).

- **Encoding:** every response is ISO-8859-1. `post()` converts it to UTF-8, and the
  XML decoder ignores the encoding declaration.
- **Login:** `POST /index.htm` with `part=3&passwd=<pw>`. No cookie is set; the RCU
  remembers the client itself, most likely by IP.
  - A wrong password returns the login form again with **HTTP 200**. Detect it by the
    presence of `name="passwd"` (`isLoginPage`).
  - Success returns the start page: product info (`Version`, `Modell`, `Namn`) and the
    meter table `<tr><td>EL-01</td><td>26795900 Wh</td><td>MB3</td></tr>`.
  - There is no logout. ElVa logs in at the start of every scrape, which is one cheap
    request.
- **Meter data:** `POST /mb.xml` returns `<data>` with flat elements; the `d="1"`
  attribute means the field is read-only in the UI.
  - The RCU keeps a **shared server-side cursor** over slots MB01 to MB08. The web UI's
    arrow buttons depend on it, which is why ElVa saves and restores it.
    - `counter=0` **resets the cursor to MB01**. This is the browser's page load, not a
      "stay where you are".
    - `counter=1` and `counter=2` step back and forward, wrapping at both ends.
    - `pos=N` jumps to MBN. An out-of-range N gives MB01, which `ReadMeter` reports as
      a mismatch.
    - The response's `<counter>` element is the slot index (N-1).
    - No read leaves the cursor untouched, so `Cursor()` steps next and then prev. The
      two steps cancel out and the second response reveals the original position.
      Restoring is `pos=orig`. Both were verified on a device, including at the MB08
      wrap edge.
  - Main fields: `mbPos` (`MB03`), `DBIpMB1_Txt` (name), `DBIMB1_Value1..6`,
    `DBIpMB1_VIFList1..6` (unit code for each value), `DBIMB1_Current/Prev{Hour,Day,Month}`,
    `DBIMB1_Time` (last time the RCU read the meter, in RCU local time with no zone;
    parsed with `KTC_TIMEZONE`),
    `DBIMB1_CommInfo` (`OK` when healthy), `DBIpMB1_Id` (meter serial number).
  - Empty values are `---`. Empty slots return `---` for everything.
  - VIF unit codes come from the `<select>` options in `mb.htm`: 1536 none, 1537 Wh,
    1538 kWh, 1540 l, 1541 m3, 1543 W, 1544 kW, 1545 l/h, 1546 m3/h, and so on
    (`vifUnits` in `parse.go`).
- **Data freshness:** the RCU reads meters over M-Bus on its own schedule
  (`DBIpMB1_Ival`=60 s). `mb.xml` returns cached values, so ElVa adds no M-Bus traffic.
- **Resolution:** the electricity total moves in 100 Wh steps, so momentary power (W)
  is the high-resolution signal and must always be stored.
- **Other pages** (`ai`, `di`, `rd`, `ms`) use the same cursor pattern via `*.xml`.
  Nothing in them is needed today.
- **Typical setup** (as in the test fixtures): `VV-01` at MB1 (Value1 = l,
  Value2 = l/h) and `EL-01` at MB3 (Value1 = Wh, Value2 = W). "VV" is Swedish
  shorthand for varmvatten (hot water); cold water is often not metered per apartment.
  Meter names, slots and value layout are configured per installation, which is why
  ElVa discovers meters and reads units from the VIF codes instead of hard-coding them.
  Electricity period values also move in 100 Wh steps.

### Unknowns (verify if it matters)

- How long a session lasts, and whether `mb.xml` works without logging in.
- Whether the cursor is global or per client. ElVa assumes global, which is the safe
  assumption. A web UI user who clicks an arrow during the roughly 1 s that a scrape
  takes can still land on an unexpected meter. This is harmless, because the user
  login is read-only (every field has `d="1"`).
- Whether the period values (`CurrentHour` and so on) are always in Value1's unit. This
  holds for the installations tested so far.

### Refreshing test fixtures

```sh
H=http://$KTC_HOST
curl -s -d "part=3&passwd=$KTC_PASSWORD" $H/index.htm > internal/ktc/testdata/start.html
curl -s -d "part=3&passwd=wrong" $H/index.htm         > internal/ktc/testdata/login.html
curl -s -d "part=3&passwd=$KTC_PASSWORD" $H/index.htm > /dev/null
curl -s -d pos=1 $H/mb.xml > internal/ktc/testdata/mb1_water.xml   # then pos=3, pos=5
```

Before committing new fixtures, **anonymize them** (rule 3): replace the RCU name
(`Namn`) and the meter serial numbers (`DBIpMB1_Id`, `DBIpMB1_MeterId`) with
placeholders. Then update the expected values in `ktc_test.go`. The fake RCU in that
file models the cursor semantics above, so keep it in sync if the device turns out to
behave differently.

## Roadmap and design decisions

**Database (implemented).** PostgreSQL schema, created at startup from `store.Schema`.
Keep the two in sync:

```sql
CREATE TABLE IF NOT EXISTS measurement (
    time       timestamptz PRIMARY KEY,  -- scrape time, whole minutes
    el_energy  bigint,                   -- electricity meter total, Wh
    el_power   integer,                  -- electricity momentary power, W
    va_volume  bigint,                   -- water meter total, l
    va_flow    real                      -- water momentary flow, l/h
);
```

- **Naming:** `<meter>_<quantity>`. Totals (energy, volume) and rates (power, flow) are
  named alike for both meters.
- **Mapping:** meters are assigned to columns by quantity. The energy/power meter
  fills `el_*`, the volume/flow meter fills `va_*`. Two meters of the same kind is a
  startup error.
- **Writes:** `INSERT ... ON CONFLICT (time) DO NOTHING`, so a restart within the same
  minute is harmless. All value columns are nullable: one meter may fail while the
  other works, and imported historical data may lack some columns.
- **`timestamptz`, not `timestamp`.**
  - A `timestamp` stores a wall-clock reading without a zone, so the zone it was
    written in is lost.
  - In autumn, 02:00-02:59 local time happens twice and can't be told apart.
  - Grafana treats a `timestamp` as UTC, so local values are drawn 1-2 h off.
  - `timestamptz` stores an exact moment, which fixes all of this.
- **Not stored:**
  - the RCU's per-period values (`current_hour` and so on), which can be computed
    from the totals, e.g. `max(el_energy) - min(el_energy)` per day;
  - `comm_status` and `meter_time`. Freshness is checked at scrape time and logged
    as a warning instead (`meterAgeProblem`, `ELVA_MAX_METER_AGE`).
- **Optional:** with `ELVA_DATABASE_URL` unset, ElVa prints JSON lines instead, which
  is useful for development and for trying it out.
- **Outages:** rows wait in an in-memory buffer (`ELVA_DATABASE_BUFFER`, default
  168h). When it is full, the oldest rows are dropped with an error log. A restart
  loses the buffer. A scrape that fails writes no row: there are no NULL rows for
  RCU outages.
- **Schema changes:** today the schema is a single idempotent `CREATE TABLE IF NOT
  EXISTS`. Before the first incompatible change, add a schema version table and
  numbered migrations.
- **Scale:** about 525 k rows per year. Plain PostgreSQL with the primary key index is
  enough; no TimescaleDB or extra indexes are needed.

**MCP server (implemented): `elva-mcp-stdio`.**
- **Separate binary:** it is read-only and started on demand by the client, while the
  scraper is a writing daemon. It has its own settings (`mcp.env`) and its own
  database role.
- **stdio transport**, typically started over SSH on the database host:
  `ssh host /opt/elva/bin/elva-mcp-stdio -env-file /opt/elva/mcp.env`. There is no
  network port and no extra authentication; access is granted by membership of the
  group `elva-mcp`, which can read `mcp.env`.
- **Tools** (all marked read-only):
  - `get_latest`;
  - `get_consumption`: deltas of the totals per `date_trunc` bucket, with a baseline
    from the last row before the range, at most 1000 buckets. After a gap longer
    than `store.MaxGap` (1 h) a bucket counts from its own first reading
    (`partial`), and the gap's consumption goes to `gaps` instead
    (`gap_electricity_kwh`, the 20 longest listed). Otherwise a period after an outage
    of weeks would show the whole outage, as 2026-11 did after v1's 96-day gap. A gap
    inside one bucket stays in it, since the consumption did happen in that period;
  - `get_timeseries`: `date_bin`, at most 2000 points;
  - `get_baseload`: per night within a local window; per week or month beyond 62 or
    366 nights;
  - `get_data_info`: gaps via `lag()`;
  - `get_schema`: the table and how consumption is computed. It is a tool, not an MCP
    resource, because many clients expose resources only through generic
    `list_resources`/`read_resource` tools that models skip;
  - `query`.

  There are no resources. Every answer except `query` and `get_schema` carries
  `earliest_data` (`min(time)`, from the primary key index), plus a note when the
  range starts before it.
- **Descriptions are search terms.** Clients that defer tools find them by keyword, so
  the server instructions and tool descriptions name the Swedish words users ask
  with (el, elförbrukning, nattförbrukning, standby, toppeffekt, senaste veckan, ...)
  and say there is no other data source.
- **Time:** `ELVA_TIMEZONE` sets the session time zone and the bucket boundaries;
  outputs are local RFC 3339.
- **Exit codes** follow `elva`: 1 for bad settings, 5 if the database can't be reached
  (or the role can't read `measurement`).
- **Measured** on 3.1 M synthetic rows (6 years) with PostgreSQL 17: each tool takes
  1-3.5 s, and the largest answers are about 10 KB (month buckets) and 30 KB (500
  timeseries points).
- **Testing:** `main_test.go` connects a real MCP client over in-memory transports and
  calls every tool. By hand:
  - **`scripts/mcp-inspect.sh`** opens the [MCP Inspector](https://github.com/modelcontextprotocol/inspector)
    web UI against the test database. Add `--cli --method tools/list` (or
    `tools/call --tool-name ... --tool-arg key=value`) for terminal use.
  - **Defaults:** the test container (`elva-test-pg`) with role `elva_ro`, password
    `ro`. Override with `ELVA_DATABASE_URL`; `ELVA_TEST_SCHEMA=<schema>` reads another
    schema, e.g. one filled with synthetic data.
  - **Creating that role** needs the table to exist, so run `elva` against the
    container once first. Then:
    `docker exec -i elva-test-pg psql -U postgres -d elva -v pw="'ro'" -f - < deploy/readonly-role.sql`.
  - **Raw stdio:** pipe JSON-RPC lines into the binary (`initialize`,
    `notifications/initialized`, `tools/call`).

**Grafana (planned)** will read directly from PostgreSQL; no dashboards exist yet.

**Deployment (implemented): a systemd service installed from a checkout.**
- **Layout:** `/opt/elva` by default, with `PREFIX` to override.
- **No binaries in git:** the target builds with `scripts/build.sh`. Go 1.21 and later
  fetch the toolchain `go.mod` asks for if the installed Go is older.
- **The unit's restart policy follows the exit codes:**
  - 2 and 5 (RCU or database unreachable) restart every 30 s with no start limit;
  - 1, 3 and 4 (config, password, meters) stop the service (`RestartPreventExitStatus`).

  Keep the exit codes in `main.go`, the unit and the README in sync.
- **Hardening:** the unit uses systemd sandboxing (ProtectSystem=strict, no
  capabilities, address families limited to IP and Unix sockets, ...).
  `systemd-analyze security elva` scores 3.0. ElVa needs only outgoing network
  connections and writes no files; keep it that way, or relax the unit deliberately.
- **`ProtectHome=yes`** means `PREFIX` must not be under `/home`; `install.sh` refuses.
- **`-check`** runs config, database and RCU startup checks and exits; `verify.sh`
  uses it.
- **`-version`** prints the build version, which is also the first line of every
  startup log.

**Testing the deploy scripts** without a real machine, in a throwaway systemd container:

```sh
# once: an image with systemd as PID 1, and a network shared with the test database
printf '%s\n' 'FROM debian:trixie' \
  'RUN apt-get update && apt-get install -y --no-install-recommends systemd systemd-sysv procps diffutils && rm -rf /var/lib/apt/lists/*' \
  'STOPSIGNAL SIGRTMIN+3' 'CMD ["/sbin/init"]' | docker build -t elva-systemd-test -
docker network create elva-test && docker network connect elva-test elva-test-pg

docker run -d --name elva-sd --privileged --cgroupns=private --tmpfs /run --tmpfs /run/lock \
  --network elva-test elva-systemd-test
# copy committed files only (never private ones), plus the binaries built by scripts/build.sh
git archive HEAD | docker exec -i elva-sd sh -c 'mkdir -p /srv/elva && tar -xf - -C /srv/elva'
docker cp bin/elva elva-sd:/srv/elva/bin/elva
docker cp bin/elva-mcp-stdio elva-sd:/srv/elva/bin/elva-mcp-stdio
docker exec -it elva-sd bash    # then: cd /srv/elva && deploy/install.sh && deploy/verify.sh ...
docker rm -f elva-sd            # when done
```

Use `ELVA_DATABASE_URL=postgres://postgres:test@elva-test-pg:5432/elva` inside the
container. Scenarios worth repeating after changing the scripts or the unit:
- first install, then verify with the sample config (expect failures);
- a real config: start, wait a minute, then verify (expect all good);
- an upgrade keeps `elva.env` byte for byte;
- a wrong password gives `failed` with 0 restarts;
- the database paused at startup gives restarts, then recovers by itself;
- MCP access: a user in group `elva-mcp` can run `elva-mcp-stdio -env-file
  /opt/elva/mcp.env`, and a user outside it gets "permission denied";
- uninstall, then `--purge`.
