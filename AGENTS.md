# AGENTS.md

Guide for anyone, human or AI agent, maintaining ElVa. User-facing documentation is in
[README.md](README.md).

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
3. **Never commit secrets.** `.env.*` is git-ignored except `.env.sample`. Keep local
   settings in a personal file such as `.env.<name>`. Tests use the fake password `secret`.
4. Use the standard library only, unless a dependency clearly pays for itself.
5. **Define every setting in the `settings` table in `cmd/elva/config.go`.** Don't call
   `os.Getenv` anywhere else. The table drives `-help` and the startup settings log,
   and secrets must not leak into logs. Mark a setting `secret: true` if it holds a
   credential; names containing PASSWORD, SECRET or TOKEN and passwords inside URLs
   are masked anyway. Also add every new setting to `.env.sample` and README.md.

## Build, test, run

Requires Go 1.24 or newer. If Go was installed from the tarball into a user directory
such as `~/.local/go`, put it on the PATH, e.g. in `~/.bashrc`:
`export PATH="$HOME/.local/go/bin:$HOME/go/bin:$PATH"`. Otherwise you get
`go: command not found`.

```sh
go vet ./... && go test ./...
go build -o bin/elva ./cmd/elva
./bin/elva -env-file .env.<name>                      # real RCU
ELVA_INTERVAL=10s ./bin/elva -env-file .env.<name>    # faster, for manual testing only
```

To check the startup failure paths: an empty env gives exit 1, `KTC_HOST` set to an
unused IP gives exit 2, and a wrong `KTC_PASSWORD` gives exit 3.

### Configuration loading

- Settings are read from the environment, which follows the 12-factor convention.
- `-env-file` is an opt-in convenience for running by hand: nothing is loaded
  automatically. Values already in the environment win over the file, so a
  deployment can override a single value.
- In production, prefer the supervisor's own mechanism, e.g. systemd
  `EnvironmentFile=` or compose `env_file:`.
- The file parser is deliberately minimal: `KEY=VALUE`, `#` comments, an optional
  `export ` prefix and surrounding quotes. It has no multi-line values or escape
  sequences.

## Layout

| Path | Contents |
|------|----------|
| `cmd/elva/main.go` | Startup check, minute loop, cursor restore, JSON output |
| `cmd/elva/config.go` | Settings table, `-env-file` loader, value sources, secret masking, `-help` text |
| `internal/ktc/client.go` | HTTP client: `Login`, `ReadMeter`, `Cursor`, `SetCursor`; error kinds `ErrUnreachable`, `ErrAuth` |
| `internal/ktc/parse.go` | Start page and `mb.xml` parsing, VIF unit table |
| `internal/ktc/testdata/` | Real responses captured from an RCU1111-1100 (firmware 4.7.13) |
| `internal/ktc/ktc_test.go` | Parser tests and a fake RCU (`newFakeRCU`) that models the session and cursor |

## How the RCU web UI works

Tested on RCU1111-1100, firmware 4.7.13. Reference: KTC user manual part 2 (linked from v1's README).

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
      Restoring is `pos=orig`. Both were verified on the device, including at the
      MB08 wrap edge.
  - Main fields: `mbPos` (`MB03`), `DBIpMB1_Txt` (name), `DBIMB1_Value1..6`,
    `DBIpMB1_VIFList1..6` (unit code for each value), `DBIMB1_Current/Prev{Hour,Day,Month}`,
    `DBIMB1_Time` (last time the RCU read the meter, in RCU local time),
    `DBIMB1_CommInfo` (`OK` when healthy), `DBIpMB1_Id` (meter serial number).
  - Empty values are `---`. Empty slots MB4 and up return `---` for everything.
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

Afterwards, update the expected values in `ktc_test.go`. The fake RCU in that file
models the cursor semantics above, so keep it in sync if the device turns out to
behave differently.

## Roadmap and planned design

**Database (next step).** Stay compatible with v1's table so old data and queries keep
working:

```sql
CREATE TABLE IF NOT EXISTS elva (date TIMESTAMP PRIMARY KEY, power INT, el INT, va INT);
-- power = EL momentary W, el = EL total Wh, va = water total l
```

Planned additions are nullable columns, so v1 rows remain valid:
- `va_flow` (l/h)
- the meter timestamps, so stale readings can be spotted

Open question: v1 used `TIMESTAMP` without a time zone as the primary key. Local time
repeats during the autumn DST change, which causes PK collisions for one hour a year.
`TIMESTAMPTZ` fixes this and existing queries keep working. Decide this when
implementing the database.

**Grafana** will read directly from PostgreSQL, replacing v1's JSP frontend.

**Deployment:** not decided yet. Options are a systemd unit running the static binary,
or docker compose alongside PostgreSQL and Grafana, as v1 did.
