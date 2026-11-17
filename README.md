# Werkstudent Radar

**Stop refreshing job boards. Let the radar tell you what's new.**

Werkstudent Radar searches job boards for working-student (*Werkstudent*) positions in Germany, remembers what it has already seen, and shows you only the jobs that are **new since your last run**.

```
$ radar -title backend -location Berlin -radius 25
Source arbeitsagentur found 143 jobs
Found 6 new jobs:
- Werkstudent Backend Development (m/w/d) | 10115 Berlin | Acme GmbH | https://...
- Working Student Software Engineering | 10997 Berlin | Example SE | https://...
...
```

Run it once a day (or let cron do it) and you'll never scroll past the same 140 listings again.

---

## Features

- 🔎 **Searches the Bundesagentur für Arbeit job board**, Germany's largest job listing, with every page fetched for you.
- 🧠 **Remembers what you've seen.** Everything goes into a small local SQLite file, so you only see what's actually new.
- ⚡ **Searches several sites at once.** Each job board runs in parallel with its own timeout. If one site is down you still get results from the others.
- 🔒 **Runs locally.** No account, no tracking, no cloud. Your search history stays on your machine.
- 📦 **One small binary.** Pure Go, no C compiler or system libraries needed. Runs on macOS, Linux and Windows.

Also in this repo: [**jobmail**](jobmail/README.md), an early prototype that reads a mailbox and builds an application tracker from it (applied → interview → offer) using a local AI model.

## Getting started

### 1. Install

You need [Go](https://go.dev/dl/) 1.27 or newer.

```sh
git clone https://github.com/<you>/werkstudent_radar.git
cd werkstudent_radar
go build -o radar ./cmd/radar
```

That gives you a `radar` binary in the current folder.

### 2. Run your first search

```sh
./radar -title "software" -location München -radius 30
```

On the first run **every** job counts as new, so expect a long list. Run it again tomorrow and you'll only see what appeared in between.

## Usage

```
radar [flags]
```

| Flag        | Default    | What it does                                                       |
|-------------|------------|--------------------------------------------------------------------|
| `-title`    | *(empty)*  | Extra search words, e.g. `backend`, `data`, `marketing`. "Werkstudent" is always added for you. |
| `-location` | *(empty)*  | City or postcode, e.g. `Berlin` or `80331`.                        |
| `-radius`   | *(none)*   | Search radius around `-location`, in km.                           |
| `-db`       | `radar.db` | Where to keep the database of seen jobs.                           |
| `-timeout`  | `5m`       | Maximum time for the whole run (e.g. `90s`, `10m`).                |

You can also set the database path with an environment variable:

```sh
export RADAR_DB=~/.local/share/radar.db
```

### Examples

```sh
# Anything Werkstudent in Hamburg and up to 20 km around it
./radar -location Hamburg -radius 20

# Data jobs anywhere in Germany
./radar -title data

# Keep separate databases for separate searches
./radar -title marketing -db marketing.db
```

### Run it automatically

Add a line to your crontab (`crontab -e`) to check every morning at 8:00 and keep a log:

```cron
0 8 * * * cd ~/werkstudent_radar && ./radar -location Berlin -radius 25 >> radar.log 2>&1
```

## FAQ

**Why did I get hundreds of "new" jobs?**
The first run has nothing to compare against, so everything is new. From the second run on you only see the difference.

**A source failed. Are my results gone?**
No. A failing job board shows up as a `Warning:` line and the others still finish normally. Jobs are only saved once everything has been collected, so nothing gets saved halfway.

**Where is my data?**
In `radar.db` (or wherever `-db` / `RADAR_DB` points). It's a normal SQLite file; open it with any SQLite viewer, or delete it to start fresh.

**Is scraping the Arbeitsagentur allowed?**
Radar uses the same public API the official [Jobsuche](https://www.arbeitsagentur.de/jobsuche/) website uses, at a normal pace. Please keep it that way and don't run it every minute.

## Roadmap

- [x] Arbeitsagentur source with pagination
- [x] Local SQLite storage with "new since last run" detection
- [x] Parallel sources with per-source timeouts
- [ ] More job boards
- [ ] Filters (remote only, exclude keywords, exclude companies)
- [ ] Notifications (e-mail / Telegram / push)
- [ ] A local web UI that combines found jobs with your applications from [jobmail](jobmail/README.md)

## License

No license chosen yet. Until there is one, all rights are reserved.
