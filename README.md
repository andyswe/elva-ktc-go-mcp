# ElVa (ELektricitet & VAtten)

ElVa records your apartment's **electricity and water consumption** from a
**KTC RCU1111 "lägenhetsnod"**, the M-Bus concentrator found in many Swedish
apartment buildings. Once a minute it reads every meter connected to the RCU and
stores the readings in **PostgreSQL**, ready for graphing in Grafana:

- **Electricity:** total energy (Wh) and **momentary power (W)**
- **Water:** total volume (l) and current flow (l/h)

The RCU has no API, so ElVa logs in to its built-in web interface exactly as a browser
would and reads the same data the web pages show. It only reads: it never changes any
setting on the RCU. It also puts the web UI back on the page it found, so it is safe to
run alongside other users of the device, such as your electricity company or someone
browsing the web UI.

This is version 2, a small Go rewrite of [elva-ktc](https://github.com/andyswe/elva-ktc)
(Java/Kotlin, with its own web frontend).

> **Status:** reading and storing in PostgreSQL work. Grafana dashboards and
> deployment are next.

## Requirements

- A KTC RCU1111 reachable over HTTP from the machine running ElVa (tested with model
  RCU1111-1100, firmware 4.7.13)
- The RCU's web UI login password (the one on its start page, not the System password)
- PostgreSQL (tested with 17). Without a database, ElVa prints JSON lines instead,
  which is useful for trying it out.
- Go 1.25 or newer to build

## Quick start

```sh
git clone https://github.com/andyswe/elva-ktc-2.git
cd elva-ktc-2
go build -o bin/elva ./cmd/elva

cp .env.sample .env.mine        # .env.* files are git-ignored
$EDITOR .env.mine               # set KTC_HOST, KTC_PASSWORD and ELVA_DATABASE_URL
./bin/elva -env-file .env.mine
```

ElVa creates its table on first start. The database user only needs permission to
create a table in that database, or the table must already exist. For a quick test
database:

```sh
docker run -d --name elva-pg -e POSTGRES_PASSWORD=secret -e POSTGRES_DB=elva \
  -p 127.0.0.1:5432:5432 postgres:17
# ELVA_DATABASE_URL=postgres://postgres:secret@127.0.0.1:5432/elva
```

ElVa produces a single static binary with no dependencies. To run it on another
machine, build it there or cross-compile it, for example
`GOOS=linux GOARCH=arm64 go build -o bin/elva ./cmd/elva` for a Raspberry Pi.

## Install as a service

On a Linux machine with systemd, ElVa runs as a service built from a checkout of this
repository. The build runs as your normal user; installing needs `sudo`.

```sh
git clone https://github.com/andyswe/elva-ktc-2.git && cd elva-ktc-2
scripts/build.sh                 # builds bin/elva with its version built in
sudo deploy/install.sh           # installs to /opt/elva and enables elva.service
sudo nano /opt/elva/elva.env     # first install only: fill in the settings
sudo systemctl start elva
sudo deploy/verify.sh            # checks installation, settings, service, RCU and database
```

| Path | |
|------|---|
| `/opt/elva/bin/elva` | The binary, owned by root |
| `/opt/elva/elva.env` | Settings, readable only by root and the service (`0640 root:elva`) |
| `/etc/systemd/system/elva.service` | The systemd unit, generated from `deploy/elva.service` |

The service runs as the unprivileged system user `elva`, with systemd's sandboxing
options enabled. Use `PREFIX=/some/dir` with the deploy scripts to install somewhere
other than `/opt/elva`.

- **Logs:** `journalctl -u elva -f`
- **Upgrade:** `git pull && scripts/build.sh && sudo deploy/install.sh`. This replaces
  the binary, keeps `elva.env` and restarts the service.
- **Check:** `sudo deploy/verify.sh` checks the files and their permissions, the
  settings, whether the service is enabled and running, and the version against your
  build. It then runs `elva -check`: a full startup check against the RCU and the
  database, as the service user with the installed settings. Finally it shows when the
  last row was stored and any warnings in the journal from the past hour. It exits
  with 1 if anything failed.
- **Uninstall:** `sudo deploy/uninstall.sh` removes the service and the binary but
  keeps `elva.env`. Add `--purge` to also remove `/opt/elva` and the `elva` user. The
  database is never touched.
- **Restarts** depend on why ElVa stopped:
  - A missing network or database (exit codes 2 and 5) is retried every 30 s, e.g.
    at boot before they are up.
  - Mistakes that retrying can't fix (bad settings, wrong password, meter problems:
    exit codes 1, 3 and 4) stop the service, which shows as `failed` in
    `systemctl status elva`.

`elva -version` prints the version, and `elva -check` runs the startup checks and
exits without scraping. The version is also the first line of every startup log.

## Configuration

Set these as environment variables or put them in a file passed with `-env-file`.
Variables already set in the environment take precedence over the file.
`./bin/elva -help` lists all settings. At startup ElVa logs every setting's value and
where it came from (`environment`, `env file`, `default` or `none`). Passwords and
other secrets are masked, including passwords inside connection URLs.

| Variable | Default | Description |
|----------|---------|-------------|
| `KTC_HOST` | *(required)* | Host or `host:port` of the RCU, e.g. `192.168.1.100` |
| `KTC_PASSWORD` | *(required)* | RCU web UI password |
| `KTC_METERS` | all meters | Comma-separated meter names to read, e.g. `EL-01,VV-01` |
| `ELVA_INTERVAL` | `1m` | Scrape interval, aligned to the wall clock (whole minutes) |
| `ELVA_TIMEOUT` | `10s` | Timeout per request to the RCU and per database operation |
| `KTC_TIMEZONE` | `Europe/Stockholm` | Time zone of the RCU's clock; its timestamps carry none |
| `ELVA_MAX_METER_AGE` | `2m` | Warn when the RCU's last read of a meter is older than this |
| `ELVA_DATABASE_URL` | *(unset)* | PostgreSQL URL, e.g. `postgres://elva:pw@localhost:5432/elva`. Unset: print JSON to stdout |
| `ELVA_DATABASE_BUFFER` | `168h` | How long to keep readings in memory while the database is unreachable |

The RCU itself reads the meters once a minute, so an interval shorter than `1m` only
repeats the same values.

## What it does

1. **Startup check.** ElVa connects to the database and creates its table if needed.
   It then logs in to the RCU, lists the meters on the start page, does one test read
   and logs which meter fills which column. If anything is wrong, it exits with a
   clear message:

   | Exit code | Meaning |
   |-----------|---------|
   | 1 | Bad or missing configuration |
   | 2 | The RCU can't be reached (wrong host, network down) |
   | 3 | The RCU rejected the password |
   | 4 | Anything else, e.g. a name in `KTC_METERS` doesn't exist, or two meters measure the same thing |
   | 5 | The database can't be reached, or the table can't be created |

2. **Every minute**, ElVa reads all meters and writes one row to the database, or
   prints one JSON line to stdout when no database is configured.
   - **Database down:** rows are kept in memory and written once it is back, for up
     to `ELVA_DATABASE_BUFFER` (one week by default). A restart of ElVa during an
     outage loses the buffered rows. Even then, consumption over the gap can still
     be computed from the totals.
   - **Logs** go to stderr; ElVa has no log file of its own. Under systemd they end up in the journal
   (`journalctl -u elva`), and in a container they go to `docker logs`.
   - **A failed scrape** is logged and retried on the next minute, so a rebooting
     RCU or a network glitch doesn't stop the service.

3. **Each reading is checked for freshness.** The RCU reads every meter about once a
   minute, so its last read is normally 0-60 s old. If it is older than
   `ELVA_MAX_METER_AGE`, or in the future, ElVa logs a warning but still uses the
   reading. Totals stay correct anyway; only the momentary power or flow may be
   outdated. A warning on every scrape usually means `KTC_TIMEZONE` is wrong.

### Database

ElVa writes one row per scrape to the table `measurement`:

```sql
CREATE TABLE measurement (
    time       timestamptz PRIMARY KEY,  -- scrape time, whole minutes
    el_energy  bigint,                   -- electricity meter total, Wh
    el_power   integer,                  -- electricity momentary power, W
    va_volume  bigint,                   -- water meter total, l
    va_flow    real                      -- water momentary flow, l/h
);
```

- **Which meter fills which column** depends on what it measures: the meter reporting
  energy and power fills `el_*`, the one reporting volume and flow fills `va_*`.
  Units are converted, so a meter configured in kWh or m3 still ends up in Wh and l.
- **Missing values are NULL**, e.g. when one meter has no reading.
- **`time` is an exact moment (`timestamptz`)**, so summer time changes cause no gaps
  or duplicates, and Grafana shows local time correctly.
- **The RCU's per-period values** (`current_day` and so on) are not stored. Compute any
  period from the totals instead:

```sql
-- electricity (kWh) and water (l) per day, in Swedish time
SELECT date_trunc('day', time AT TIME ZONE 'Europe/Stockholm') AS day,
       (max(el_energy) - min(el_energy)) / 1000.0 AS kwh,
       max(va_volume) - min(va_volume)             AS litres
FROM measurement GROUP BY 1 ORDER BY 1;

-- power curve for Grafana (time series panel)
SELECT time, el_power FROM measurement WHERE $__timeFilter(time) ORDER BY time;
```

### JSON output (without a database)

When `ELVA_DATABASE_URL` is unset, ElVa prints each scrape as one compact JSON line on
stdout. This output contains everything the RCU reports, including the per-period
values. Readings are the only thing on stdout; all logs go to stderr, so the two can be
sent to different places:

```sh
./bin/elva -env-file .env.mine > readings.jsonl 2> elva.log
./bin/elva -env-file .env.mine 2>/dev/null | jq '.meters[] | {name, values}'
```

Here is one scrape, pretty-printed with explanations (real output has no comments).
It was taken at 13:28 on 22 November.

```jsonc
{
  // When ElVa ran this scrape (local time with UTC offset), rounded to the interval,
  // so normally a whole minute. Use it as the timestamp of the measurement.
  "time": "2026-11-22T13:28:00+01:00",

  // One entry per meter connected to the RCU, in the order of the RCU start page.
  "meters": [
    {
      // RCU M-Bus slot the meter sits in: 1 means object "MB1" in the RCU web UI.
      "pos": 1,
      // Name configured on the RCU. "VV" is the Swedish abbreviation for varmvatten,
      // so this is most likely a hot-water meter. Cold water isn't metered here.
      "name": "VV-01",
      // The meter's own M-Bus serial number (secondary address), printed on the meter.
      "meter_id": "12345678",
      // When the RCU last read this meter over M-Bus. The RCU does this about once a
      // minute on its own schedule, so the value is 0-60 s older than "time". If it
      // stops advancing, the RCU has lost contact with the meter, and ElVa logs a
      // warning (see ELVA_MAX_METER_AGE).
      "meter_time": "2026-11-22T13:28:13+01:00",
      // Result of the RCU's last M-Bus read. "OK" when healthy; anything else is
      // logged as a warning.
      "comm_status": "OK",

      // The meter's live readings. The RCU has six value slots per meter; only
      // configured slots that hold a value are listed.
      "values": [
        {
          "index": 1,             // value slot on the RCU (Value1..Value6)
          "quantity": "volume",   // what it measures
          "unit": "l",            // unit, as configured on the RCU
          "value": 187588         // meter total since installation: 187 588 l = 187.6 m3
        },
        {
          "index": 2,
          "quantity": "flow",
          "unit": "l/h",
          "value": 1              // momentary flow right now, in litres per hour
        }
      ],

      // Consumption per calendar period, calculated by the RCU from the total.
      // Same unit as value slot 1 (litres here). "current_*" is the period so far;
      // "prev_*" is the whole previous period. A field is left out if the RCU has
      // no value for it.
      "current_hour": 0,          // l used since 13:00
      "current_day": 2,           // l used since midnight
      "current_month": 986,       // l used since 1 November
      "prev_hour": 2,             // l used 12:00-13:00
      "prev_day": 0,              // l used yesterday
      "prev_month": 1349          // l used in October
    },
    {
      "pos": 3,                   // RCU slot "MB3"
      "name": "EL-01",            // electricity meter for the apartment
      "meter_id": "87654321",
      "meter_time": "2026-11-22T13:28:10+01:00",
      "comm_status": "OK",
      "values": [
        {
          "index": 1,
          "quantity": "energy",
          "unit": "Wh",
          // Meter total: 26 796 100 Wh = 26 796.1 kWh. It only moves in 100 Wh steps,
          // too coarse for minute-by-minute graphs, so use "power" for those.
          "value": 26796100
        },
        {
          "index": 2,
          "quantity": "power",
          "unit": "W",
          // Momentary power right now. This is the high-resolution signal: one
          // sample per minute of the actual load.
          "value": 512
        }
      ],
      // Per-period consumption in Wh (same unit as slot 1), so also in 100 Wh steps.
      "current_hour": 300,        // 0.3 kWh since 13:00
      "current_day": 2400,        // 2.4 kWh since midnight
      "current_month": 162300,    // 162.3 kWh since 1 November
      "prev_hour": 300,           // 0.3 kWh 12:00-13:00
      "prev_day": 3800,           // 3.8 kWh yesterday
      "prev_month": 242900        // 242.9 kWh in October
    }
  ]
}
```

Possible `quantity`/`unit` pairs, depending on how a meter is set up on the RCU:

| quantity | units |
|----------|-------|
| `energy` | Wh, kWh, MWh |
| `power` | W, kW |
| `volume` | l, m3 |
| `flow` | l/h, m3/h |
| `mass` | kg |
| `flow_temp`, `return_temp`, `external_temp` | C |
| `temp_diff` | K |
| `pressure` | bar |
| `vif_<code>` | *(empty)*, for an RCU unit code ElVa doesn't know yet |

## Roadmap

- [x] Read electricity and water from the RCU; startup connectivity and auth check
- [x] Store in PostgreSQL
- [ ] Grafana dashboards
- [x] systemd service with install, verify and uninstall scripts

## License

MIT, see [LICENSE](LICENSE).
