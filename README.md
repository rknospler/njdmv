# NJ MVC Appointment Finder

Monitors NJ Motor Vehicle Commission appointment availability across multiple locations and alerts you when early openings appear.

## How It Works

Scrapes the [NJ MVC appointment portal](https://telegov.njportal.com/njmvc/AppointmentWizard) for the earliest available appointments across NJ MVC locations. Supports 11 service types (license renewal, Real ID, registration, CDL, etc.) — each with its own set of locations and site IDs, all discovered dynamically from the portal at startup. Fetches are concurrent for speed.

Each run:
1. Fetches the service index page to discover which locations offer that service
2. Fetches the earliest appointment date from each location concurrently
3. Sorts results by date (soonest first)
4. Compares against the previous run and shows diff markers (⬇️ earlier, ⬆️ later, ✨ new, ❌ removed)
5. Color-codes output: **green** (< 3 days), **yellow** (< 7 days), **red** (7+ days)
6. Triggers alerts (sound + email/SMS) for appointments within 3 days

## Usage

Either `-zip` or `-all` is required. Use `-service` to pick the appointment type (default: `renewal`).

```bash
# Search all locations for license renewal (default)
./findappt -all

# Registration renewal at all locations
./findappt -all -service registration

# Real ID within 25 miles of a zip code
./findappt -zip 07869 -radius 25 -service realid

# Search with SMS notification
./findappt -all -service title -notify 5551234567@tmomail.net

# List locations that offer a specific service
./findappt -list -service registration

# List all available service types
./findappt -services
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-service` | `renewal` | Service type (`renewal`, `realid`, `registration`, `title`, `permit`, etc.) |
| `-zip` | *(none)* | Center zip code for radius-based search |
| `-radius` | `30` | Search radius in miles (used with `-zip`) |
| `-all` | `false` | Search all locations for the selected service |
| `-notify` | *(none)* | Email/SMS gateway address for alerts (e.g. `5551234567@tmomail.net`) |
| `-list` | `false` | Print locations for the selected service and exit |
| `-services` | `false` | List all available service types and exit |

### Service Types

| Name | Description |
|------|-------------|
| `renewal` | License / Non-Driver ID Renewal |
| `realid` | Real ID |
| `permit` | Initial Permit (before knowledge test) |
| `title` | New Title or Registration |
| `registration` | Registration Renewal |
| `replace-title` | Replacement Title |
| `transfer` | Transfer from Out of State |
| `cdl-permit` | CDL Permit or Endorsement |
| `non-driver` | Non-Driver ID (new) |
| `knowledge` | Knowledge Test (non-CDL) |
| `cdl-knowledge` | CDL Knowledge Test |

Not all locations offer every service. The tool dynamically discovers which locations are available for your chosen service type.

When using `-zip`, the program geocodes the zip code via [zippopotam.us](http://api.zippopotam.us) (free, no key required), then filters to locations within the radius using the Haversine formula. Distance in miles is shown next to each result.

## Alerts

- Plays a system sound (`Glass.aiff`) when an appointment is found within 3 days
- When `-notify` is set, sends an email/SMS via `mail` to the given address
- Tracks alerts per appointment key and suppresses after 8 consecutive alerts for the same slot

## Build & Run

```bash
go build -o findappt findappt.go
./findappt -all
```

Run on a loop with `watch`:

```bash
watch -n 30 ./findappt -all
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
- `mail` command (Postfix) for email/SMS — see below

## Email / SMS Setup

The `-notify` flag requires a working `mail` command, which on macOS uses Postfix. Run `setup_postfix.sh` to configure Postfix as an SMTP relay through any provider:

```bash
# Interactive — prompts for host, user, password
./setup_postfix.sh

# Non-interactive — pass everything via flags
./setup_postfix.sh \
  --smtp-host smtp.gmail.com \
  --smtp-port 587 \
  --smtp-user you@gmail.com \
  --smtp-pass 'xxxx xxxx xxxx xxxx' \
  --test-addr 5551234567@tmomail.net
```

Run `./setup_postfix.sh --help` for all options.

**Common SMTP hosts:**

| Provider | Host | Port | Notes |
|----------|------|------|-------|
| Gmail | `smtp.gmail.com` | 587 | Requires [App Password](https://myaccount.google.com/apppasswords) |
| Outlook | `smtp.office365.com` | 587 | |
| Yahoo | `smtp.mail.yahoo.com` | 587 | |
| iCloud | `smtp.mail.me.com` | 587 | Requires App Password |

**SMS via email gateway:** Most carriers accept email-to-SMS. Pass the gateway address to `-notify`:

| Carrier | Gateway format |
|---------|---------------|
| T-Mobile | `<number>@tmomail.net` |
| Verizon | `<number>@vtext.com` |
| AT&T | `<number>@txt.att.net` |

## Rate Limiting / Responsible Use

**Do not run this tool too frequently.** Each invocation makes one request per location (20–28 HTTP requests depending on the service), plus one request to discover the location list. Running it every 30–60 seconds is reasonable; running it every few seconds will likely get your IP temporarily or permanently banned by the NJ MVC portal.

If you are using `watch`, keep the interval at **30 seconds or more**:

```bash
watch -n 60 ./findappt -all
```

This tool is for personal use. Be respectful of the state's infrastructure.
