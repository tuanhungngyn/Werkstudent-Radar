#!/usr/bin/env python3
"""
jobmail: build an application tracker from your mailbox with a local LLM.

Reads iCloud Mail over IMAP (read-only, nothing is marked as read or changed),
pre-filters job-related mails, lets a local Ollama model classify each one
into structured JSON, and writes:
  - applications.csv / .xlsx : one row per company + position, latest status
  - events.csv               : every classified mail (for checking)
  - dashboard.html           : if dashboard.py sits next to this script
Processed mails are cached in jobmail.db, so re-runs only look at new mail.

Usage:
  python3 jobmail.py                  # default: mails since 2026-06-01
  python3 jobmail.py --since 2026-09-01 --mailboxes INBOX Archive
"""
from __future__ import annotations

import argparse
import csv
import email
import email.utils
import html
import imaplib
import json
import os
import re
import sqlite3
import subprocess
import sys
import urllib.request
from datetime import datetime
from email.header import decode_header, make_header
from pathlib import Path

# ---------------------------------------------------------------- config
IMAP_HOST = "imap.mail.me.com"          # iCloud Mail
IMAP_PORT = 993
OLLAMA_URL = "http://localhost:11434/api/chat"
MODEL = os.environ.get("JOBMAIL_MODEL", "qwen3:8b")   # qwen3:4b = lighter, qwen3:14b = heavier
KEYCHAIN_SERVICE = "jobmail-imap"       # macOS Keychain entry name
MAX_BODY_CHARS = 1500                   # first part of a mail is enough to classify it
NUM_CTX = 4096                          # small context window = small KV cache = less RAM
FETCH_CHUNK = 200                       # headers fetched per IMAP request

# Senders that are almost always job-related (applicant tracking systems, job boards)
ATS_SENDER_HINTS = [
    "softgarden", "personio", "join.com", "greenhouse", "lever.co", "workday",
    "myworkday", "successfactors", "smartrecruiters", "recruitee", "ashby",
    "stepstone", "indeed", "linkedin", "xing", "workingstudentjobs",
    "talent", "recruiting", "karriere", "career", "jobs@", "hr@",
]


def load_companies() -> dict:
    """Your own company lists live in companies.json next to this script (git-ignored,
    see companies.example.json). Missing file = empty lists, the tool still works."""
    path = Path(__file__).with_name("companies.json")
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {}
    except json.JSONDecodeError as err:
        sys.exit(f"companies.json is not valid JSON: {err}")


# Companies you applied to: their mails count as candidates even with generic subjects.
# Lowercase, matched against sender and subject. Set "hints" in companies.json.
COMPANY_HINTS = [h.lower() for h in load_companies().get("hints", [])]
# Words in subject/body that suggest a job mail
KEYWORDS = [
    "bewerbung", "application", "applied", "vorstellungsgespräch", "interview",
    "werkstudent", "working student", "absage", "leider", "unfortunately",
    "eingangsbestätigung", "kennenlernen", "stelle", "position", "candidate",
    "kandidat", "zusage", "angebot", "offer",
]

STATUS_RANK = {  # later stages win when merging events into one application
    "application_confirmation": 1,
    "recruiter_outreach": 1,
    "other": 0,
    "interview_invite": 3,
    "rejection": 4,
    "offer": 5,
}

LLM_SCHEMA = {
    "type": "object",
    "properties": {
        "is_job_related": {"type": "boolean"},
        "company": {"type": "string"},
        "position": {"type": "string"},
        "type": {
            "type": "string",
            "enum": ["application_confirmation", "rejection", "interview_invite",
                     "offer", "recruiter_outreach", "other"],
        },
        "summary": {"type": "string"},
    },
    "required": ["is_job_related", "company", "position", "type", "summary"],
}

SYSTEM_PROMPT = """You classify emails for a student's job-application tracker.
Decide if the email is about one of HIS job applications or a recruiter contacting him.
Newsletters, job alerts listing many jobs, and marketing are NOT job related.
Return JSON only:
- is_job_related: true/false
- company: the hiring company (not the ATS like softgarden/personio), short name, e.g. "Acme"
- position: job title as written, or "" if unknown
- type: application_confirmation | rejection | interview_invite | offer | recruiter_outreach | other
- summary: one short sentence in German or English, e.g. "Einladung zum Interview am 12.03."
If not job related, use company "", position "", type "other"."""


# ---------------------------------------------------------------- credentials
def get_credentials() -> tuple[str, str]:
    user = os.environ.get("JOBMAIL_USER")
    pw = os.environ.get("JOBMAIL_APP_PASSWORD")
    if not user:
        sys.exit("Set JOBMAIL_USER to your iCloud mail address (see README).")
    if not pw and sys.platform == "darwin":
        try:
            pw = subprocess.run(
                ["security", "find-generic-password", "-s", KEYCHAIN_SERVICE, "-a", user, "-w"],
                capture_output=True, text=True, check=True,
            ).stdout.strip()
        except subprocess.CalledProcessError:
            pw = None
    if not pw:
        sys.exit("No app-specific password found (Keychain entry or JOBMAIL_APP_PASSWORD).")
    return user, pw


# ---------------------------------------------------------------- mail parsing
def decode(value: str | None) -> str:
    if not value:
        return ""
    try:
        return str(make_header(decode_header(value)))
    except Exception:
        return value


def html_to_text(raw: str) -> str:
    raw = re.sub(r"(?is)<(script|style).*?</\1>", " ", raw)
    raw = re.sub(r"(?i)<br\s*/?>|</p>|</div>", "\n", raw)
    raw = re.sub(r"<[^>]+>", " ", raw)
    return html.unescape(raw)


def body_text(msg: email.message.Message) -> str:
    plain, htmlpart = None, None
    for part in msg.walk() if msg.is_multipart() else [msg]:
        if part.get_content_maintype() == "multipart" or part.get_filename():
            continue
        try:
            payload = part.get_payload(decode=True)
            if payload is None:
                continue
            text = payload.decode(part.get_content_charset() or "utf-8", errors="replace")
        except Exception:
            continue
        if part.get_content_type() == "text/plain" and plain is None:
            plain = text
        elif part.get_content_type() == "text/html" and htmlpart is None:
            htmlpart = text
    text = plain if plain else html_to_text(htmlpart or "")
    return re.sub(r"\s+", " ", text).strip()


def looks_job_related(sender: str, subject: str, body: str = "") -> bool:
    s, txt = sender.lower(), f"{subject} {body[:1500]}".lower()
    return (any(h in s for h in ATS_SENDER_HINTS)
            or any(c in s or c in txt for c in COMPANY_HINTS)
            or any(k in txt for k in KEYWORDS))


# ---------------------------------------------------------------- LLM
def classify(sender: str, subject: str, date: str, body: str) -> dict:
    prompt = f"From: {sender}\nDate: {date}\nSubject: {subject}\n\n{body[:MAX_BODY_CHARS]}"
    req = {
        "model": MODEL,
        "messages": [{"role": "system", "content": SYSTEM_PROMPT},
                     {"role": "user", "content": prompt}],
        "format": LLM_SCHEMA,
        "stream": False,
        "think": False,
        "options": {"temperature": 0, "num_ctx": NUM_CTX},
        "keep_alive": "2m",   # stay loaded during the run, unload 2 min after
    }
    data = json.dumps(req).encode()
    r = urllib.request.Request(OLLAMA_URL, data=data, headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(r, timeout=300) as resp:
        out = json.loads(resp.read())
    return json.loads(out["message"]["content"])


# ---------------------------------------------------------------- storage
def open_db(path: Path) -> sqlite3.Connection:
    db = sqlite3.connect(path)
    db.execute("""CREATE TABLE IF NOT EXISTS mails(
        message_id TEXT PRIMARY KEY, mailbox TEXT, date TEXT, sender TEXT, subject TEXT,
        prefiltered INTEGER, is_job INTEGER, company TEXT, position TEXT, type TEXT, summary TEXT)""")
    return db


def normalize_company(name: str) -> str:
    n = name.lower()
    n = re.sub(r"\b(gmbh|ag|se|kg|kgaa|co|mbh|ug|inc|ltd|group|gruppe)\b", " ", n)
    n = re.sub(r"[^a-z0-9äöüß]+", "", n)
    return n


def build_tracker(db: sqlite3.Connection) -> list[dict]:
    rows = db.execute("""SELECT date, company, position, type, summary, subject
                         FROM mails WHERE is_job=1 AND company<>'' ORDER BY date""").fetchall()
    apps: dict[str, dict] = {}
    for date, company, position, typ, summary, subject in rows:
        key = normalize_company(company)
        a = apps.setdefault(key, {"company": company, "position": position, "first_contact": date,
                                  "last_update": date, "status": typ, "last_summary": summary,
                                  "mails": 0, "last_subject": subject})
        a["mails"] += 1
        if position and not a["position"]:
            a["position"] = position
        # newest mail wins; on the same day the later stage wins
        if date > a["last_update"] or (date == a["last_update"]
                                       and STATUS_RANK.get(typ, 0) >= STATUS_RANK.get(a["status"], 0)):
            a.update(last_update=date, status=typ, last_summary=summary, last_subject=subject)
    return sorted(apps.values(), key=lambda a: a["last_update"], reverse=True)


def cleaned_apps(outdir: Path) -> list[dict] | None:
    """Rows from dashboard.py's cleanup (noise dropped, names merged, overrides
    applied), so the spreadsheet matches the dashboard. None if dashboard.py is missing."""
    try:
        sys.path.insert(0, str(Path(__file__).resolve().parent))
        import dashboard
    except ImportError:
        return None
    apps, _ = dashboard.tracker(outdir / "jobmail.db")
    rows = []
    for a in apps:
        last = a["events"][-1] if a["events"] else {}
        rows.append({
            "company": a["company"], "position": a["position"], "status": a["status"],
            "after_interview": "yes" if a.get("after_interview") else "",
            "is_application": "yes" if a["is_application"] else "no (contact)",
            "first_contact": a["applied"], "last_update": a["last"], "days_quiet": a["quiet"],
            "mails": len(a["events"]), "note": a.get("note", ""),
            "last_summary": last.get("summary", ""), "last_subject": last.get("subject", ""),
        })
    return rows


def write_outputs(db: sqlite3.Connection, outdir: Path) -> None:
    apps = cleaned_apps(outdir)
    if apps is not None:
        cols = ["company", "position", "status", "after_interview", "is_application",
                "first_contact", "last_update", "days_quiet", "mails", "note",
                "last_summary", "last_subject"]
    else:  # no dashboard.py: raw per-company view
        apps = build_tracker(db)
        cols = ["company", "position", "status", "first_contact", "last_update", "mails",
                "last_summary", "last_subject"]
    with open(outdir / "applications.csv", "w", newline="", encoding="utf-8-sig") as f:
        w = csv.DictWriter(f, fieldnames=cols)
        w.writeheader()
        w.writerows({c: a[c] for c in cols} for a in apps)
    ev = db.execute("""SELECT date, company, position, type, summary, sender, subject, mailbox
                       FROM mails WHERE is_job=1 ORDER BY date DESC""").fetchall()
    ev_cols = ["date", "company", "position", "type", "summary", "sender", "subject", "mailbox"]
    with open(outdir / "events.csv", "w", newline="", encoding="utf-8-sig") as f:
        w = csv.writer(f)
        w.writerow(ev_cols)
        w.writerows(ev)
    try:
        from openpyxl import Workbook
        wb = Workbook()
        ws = wb.active
        ws.title = "Applications"
        ws.append(cols)
        for a in apps:
            ws.append([a[c] for c in cols])
        ws2 = wb.create_sheet("Events")
        ws2.append(ev_cols)
        for r in ev:
            ws2.append(list(r))
        for sheet in (ws, ws2):
            sheet.freeze_panes = "A2"
            sheet.auto_filter.ref = sheet.dimensions
        wb.save(outdir / "applications.xlsx")
        print(f"Wrote applications.xlsx ({len(apps)} rows, {len(ev)} mails)")
    except ImportError:
        print(f"Wrote CSVs ({len(apps)} rows). pip install openpyxl for an .xlsx file.")


# ---------------------------------------------------------------- main
def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--since", default="2026-06-01", help="YYYY-MM-DD")
    ap.add_argument("--mailboxes", nargs="+", default=["INBOX"])
    ap.add_argument("--out", default=".")
    args = ap.parse_args()

    outdir = Path(args.out)
    outdir.mkdir(parents=True, exist_ok=True)
    db = open_db(outdir / "jobmail.db")
    user, pw = get_credentials()
    since = datetime.strptime(args.since, "%Y-%m-%d").strftime("%d-%b-%Y")

    imap = imaplib.IMAP4_SSL(IMAP_HOST, IMAP_PORT)
    imap.login(user, pw)
    try:
        for box in args.mailboxes:
            status, _ = imap.select(f'"{box}"', readonly=True)  # read-only: nothing changes
            if status != "OK":
                print(f"Mailbox {box!r} not found, skipping.")
                continue
            _, data = imap.search(None, "SINCE", since)
            ids = data[0].split()
            print(f"{box}: {len(ids)} mails since {args.since}")

            # Step 1: headers only, in batches (fast), to pick candidates
            candidates = []
            for i in range(0, len(ids), FETCH_CHUNK):
                chunk = b",".join(ids[i:i + FETCH_CHUNK])
                _, resp = imap.fetch(chunk, "(BODY.PEEK[HEADER.FIELDS (FROM SUBJECT DATE MESSAGE-ID)])")
                for part in resp:
                    if not isinstance(part, tuple):
                        continue
                    num = part[0].split()[0]
                    h = email.message_from_bytes(part[1])
                    mid = (h.get("Message-ID") or "").strip() or f"{box}:{num.decode()}"
                    if db.execute("SELECT 1 FROM mails WHERE message_id=?", (mid,)).fetchone():
                        continue
                    sender, subject = decode(h.get("From")), decode(h.get("Subject"))
                    try:
                        date = email.utils.parsedate_to_datetime(h.get("Date")).strftime("%Y-%m-%d")
                    except Exception:
                        date = ""
                    if looks_job_related(sender, subject):
                        candidates.append((num, mid, sender, subject, date))
                    else:
                        db.execute("INSERT INTO mails VALUES (?,?,?,?,?,0,0,'','','','')",
                                   (mid, box, date, sender, subject))
                db.commit()
            print(f"  {len(candidates)} candidates for the LLM")

            # Step 2: full mail + LLM only for candidates
            for n, (num, mid, sender, subject, date) in enumerate(candidates, 1):
                _, msg_data = imap.fetch(num, "(BODY.PEEK[])")  # PEEK: does not set \\Seen
                raw = next((p[1] for p in msg_data if isinstance(p, tuple)), None)
                if raw is None:
                    continue
                text = body_text(email.message_from_bytes(raw))
                try:
                    r = classify(sender, subject, date, text)
                except Exception as e:  # Ollama down or bad JSON: retry on next run
                    print(f"  ! LLM failed on '{subject[:60]}': {e}")
                    continue
                db.execute("INSERT INTO mails VALUES (?,?,?,?,?,1,?,?,?,?,?)",
                           (mid, box, date, sender, subject, int(r["is_job_related"]),
                            r["company"], r["position"], r["type"], r["summary"]))
                db.commit()
                mark = f"{r['company']}: {r['type']}" if r["is_job_related"] else "not job-related"
                print(f"  [{n}/{len(candidates)}] {date} {mark}")
            db.commit()
    finally:
        imap.logout()
    write_outputs(db, outdir)
    try:  # dashboard.py next to this script -> refresh dashboard.html too
        sys.path.insert(0, str(Path(__file__).resolve().parent))
        import dashboard
        print(f"Dashboard: {dashboard.build(outdir / 'jobmail.db', outdir / 'dashboard.html').resolve()}")
    except ImportError:
        pass


if __name__ == "__main__":
    main()