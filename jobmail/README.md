# jobmail

> ⚠️ **Prototype.** This is built around one setup (iCloud Mail, macOS Keychain, a local Ollama model) and isn't meant as a general tool yet. It will eventually be merged into [Werkstudent Radar](../README.md).

**Your inbox already knows where you applied. jobmail turns it into a tracker.**

jobmail reads a mailbox, lets a local AI model pick out and classify the job-related mails (confirmation, rejection, interview, offer, recruiter), and builds an overview with one row per application:

| Company    | Position             | Status       | Last mail  |
|------------|----------------------|--------------|------------|
| Acme       | Werkstudent Backend  | 🟢 Interview  | 2026-10-12 |
| Example SE | Working Student Data | 🔴 Rejected   | 2026-10-03 |
| Foo GmbH   | Werkstudent IT       | ⚪ No reply   | 2026-09-01 |

Everything runs locally. Mail is opened read-only, and no mail content leaves the machine.

## What it does

- Scans IMAP folders **read-only** and pre-filters likely job mails by sender and subject
- Classifies each one with **Ollama** (`qwen3:8b` by default) into structured JSON
- Drops job alerts and portal noise, merges company spellings, and marks applications with no answer after 28 days as *No reply*
- Caches processed mails in `jobmail.db`, so re-runs only look at new mail
- Writes `dashboard.html` (one offline file), `applications.csv`/`.xlsx` and `events.csv`

## Running it

What it currently assumes:

- An iCloud Mail account with an [app-specific password](https://support.apple.com/en-us/102654), stored in the macOS Keychain under the service name `jobmail-imap` (or set `JOBMAIL_APP_PASSWORD` instead)
- Ollama running on `localhost:11434` with the model pulled
- Python 3.9+, plus `openpyxl` if you want the `.xlsx` file

```sh
ollama pull qwen3:8b
security add-generic-password -s jobmail-imap -a you@icloud.com -w

export JOBMAIL_USER=you@icloud.com
python3 jobmail.py --since 2026-09-01     # options: --mailboxes INBOX Archive, --out DIR
python3 dashboard.py --open               # rebuild the dashboard, e.g. after editing overrides
```

Set `JOBMAIL_MODEL=qwen3:4b` for a lighter model or `qwen3:14b` for better results.

## Manual fixes

Both files are optional and git-ignored.

- **`companies.json`**: company hints, sender domain → company, and spelling aliases. See `companies.example.json`.
- **`overrides.json`**: fixes what the mails don't say:

  ```json
  {
    "Acme": { "position": "Werkstudent Backend", "note": "Interview 12.03." },
    "Some GmbH": { "position": "Werkstudent IT", "status": "Declined", "applied": "2026-09-14" }
  }
  ```

  Keys are `position`, `status`, `note` and `applied` (`applied` adds an application that isn't in the mails at all). Use `"Company|part of position"` to target one of several positions at the same company. The full list of statuses is in the docstring at the top of `dashboard.py`.

## Known limitations

- iCloud only (`IMAP_HOST` is hardcoded at the top of `jobmail.py`)
- The Keychain lookup only works on macOS
- The noise filters and sender hints are tuned to one inbox
- The default `--since` date is hardcoded
