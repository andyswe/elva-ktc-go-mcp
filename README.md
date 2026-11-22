# ElVa (ELektricitet & VAtten)

ElVa records your apartment's **electricity and water consumption** from a
**KTC RCU1111 "lägenhetsnod"**, the M-Bus concentrator found in many Swedish
apartment buildings. Once a minute it reads every meter connected to the RCU:

- **Electricity:** total energy (Wh) and **momentary power (W)**
- **Water:** total volume (l) and current flow (l/h)
- consumption for the current and previous hour, day and month, as calculated by the RCU

The RCU has no API, so ElVa logs in to its built-in web interface exactly as a browser
would and reads the same data the web pages show. It only reads: it never changes any
setting on the RCU. It also puts the web UI back on the page it found, so it is safe to
run alongside other users of the device, such as your electricity company or someone
browsing the web UI.

This is version 2, a small Go rewrite of [elva-ktc](https://github.com/andyswe/elva-ktc)
(Java/Kotlin, with its own web frontend).

> **Status:** measurements are printed to stdout as JSON lines. Storing them in
> PostgreSQL and graphing with Grafana is the next step.

## Requirements

- A KTC RCU1111 reachable over HTTP from the machine running ElVa (tested with model
  RCU1111-1100, firmware 4.7.13)
- The RCU's web UI login password (the one on its start page, not the System password)
- Go 1.24 or newer to build

## Quick start

```sh
git clone https://github.com/andyswe/elva-ktc-2.git
cd elva-ktc-2
go build -o bin/elva ./cmd/elva

cp .env.sample .env.mine        # .env.* files are git-ignored
$EDITOR .env.mine               # set KTC_HOST and KTC_PASSWORD
./bin/elva -env-file .env.mine
```

ElVa produces a single static binary with no dependencies. To run it on another
machine, build it there or cross-compile it, for example
`GOOS=linux GOARCH=arm64 go build -o bin/elva ./cmd/elva` for a Raspberry Pi.

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
| `ELVA_TIMEOUT` | `10s` | Timeout per HTTP request to the RCU |

The RCU itself reads the meters once a minute, so an interval shorter than `1m` only
repeats the same values.

## What it does

1. **Startup check.** ElVa logs in, lists the meters on the RCU start page and does
   one test read. If anything is wrong, it exits with a clear message:

   | Exit code | Meaning |
   |-----------|---------|
   | 1 | Bad or missing configuration |
   | 2 | The RCU can't be reached (wrong host, network down) |
   | 3 | The RCU rejected the password |
   | 4 | Anything else, e.g. a name in `KTC_METERS` doesn't exist |

2. **Every minute**, ElVa reads all meters and prints one JSON line to stdout. Logs go
   to stderr. A failed scrape is logged and retried on the next minute, so a rebooting
   RCU or a network glitch doesn't stop the service.

### Output format

Each line is one compact JSON object. Here is one scrape, pretty-printed with
explanations (real output has no comments). It was taken at 13:28 on 22 November.

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
      // stops advancing, the RCU has lost contact with the meter.
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
- [ ] Store in PostgreSQL (schema compatible with v1's `elva` table)
- [ ] Grafana dashboards
- [ ] Container or systemd deployment

## License

MIT, see [LICENSE](LICENSE).
