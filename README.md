# NJ MVC Appointment Finder

Monitors NJ Motor Vehicle Commission appointment availability across multiple locations and alerts you when early openings appear.

## How It Works

Scrapes the [NJ MVC appointment portal](https://telegov.njportal.com/njmvc/AppointmentWizard/11) for the earliest available "Initial Permit" appointments at up to 28 NJ MVC locations. Fetches are concurrent for speed.

Each run:
1. Fetches the earliest appointment date from each site
2. Sorts results by date (soonest first)
3. Compares against the previous run and shows diff markers (⬇️ earlier, ⬆️ later, ✨ new, ❌ removed)
4. Color-codes output: **green** (< 3 days), **yellow** (< 7 days), **red** (7+ days)
5. Triggers alerts (sound + email/SMS) for appointments within 3 days

## Usage

```bash
# Default: search the original 9 northern NJ locations
./findappt

# Search all 28 NJ MVC locations
./findappt -all

# Search within 25 miles of a zip code
./findappt -zip 07869 -radius 25

# Search within default 30 miles of a zip code
./findappt -zip 07114

# List all known MVC locations and exit
./findappt -list
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-zip` | *(none)* | Center zip code for radius-based search |
| `-radius` | `30` | Search radius in miles (used with `-zip`) |
| `-all` | `false` | Search all 28 NJ MVC locations |
| `-list` | `false` | Print all known locations and exit |

When using `-zip`, the program geocodes the zip code via [zippopotam.us](http://api.zippopotam.us) (free, no key required), then filters to locations within the radius using the Haversine formula. Distance in miles is shown next to each result.

## Alerts

- Plays a system sound (`Glass.aiff`) when an appointment is found within 3 days
- Sends an email/SMS via `mail` to a configured T-Mobile gateway address
- Tracks alerts per appointment key and suppresses after 8 consecutive alerts for the same slot

## Build & Run

```bash
go build -o findappt findappt.go
./findappt
```

Run on a loop with `watch`:

```bash
watch -n 30 ./findappt
```

## State Files

| File | Purpose |
|------|---------|
| `/tmp/findappt_buffer.txt` | Last 3 runs for diff comparison and history display |
| `/tmp/findappt_alert_counts.txt` | Tracks consecutive alert count per appointment |
| `/tmp/findappt_emails_sent.txt` | Prevents duplicate email/SMS for the same appointment |

## Dependencies

- Go 1.21+
- macOS `afplay` for sound alerts
- `mail` command (Postfix) for email/SMS — see `setup_postfix.sh`
