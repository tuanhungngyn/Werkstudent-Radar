#!/usr/bin/env python3
"""
dashboard: turn jobmail.db into one self-contained dashboard.html.

  python3 dashboard.py               # reads ./jobmail.db, writes ./dashboard.html
  python3 dashboard.py --open        # ...and opens it in your browser
  python3 dashboard.py --out ~/jobs  # if jobmail.db lives somewhere else

jobmail.py calls this automatically after each run when both files sit in the
same folder. Nothing leaves your machine: no CDN, no fonts, no tracking.

What it cleans up on top of the LLM output:
  - drops job alerts and portal noise (Glassdoor, Stepstone job agent,
    "this job is a match", LinkedIn feed mails, account/password mails)
  - merges company spellings (Acme SE -> Acme, ...; see companies.json)
  - groups mails into one application per company + position (job IDs win)
  - status = what actually happened: an "other" mail never hides a rejection
  - no reply for GHOST_DAYS days = "No reply"

Optional overrides.json next to jobmail.db fixes what the mails don't say:
  {"Acme": {"position": "Werkstudent Backend",
            "note": "Interview 12.03."}}
Keys: position, status (Waiting, Action needed, Interview, Rejected, Offer,
Declined, Talent pool, No reply), note. "Action needed" is set automatically
when the newest mail asks you for documents etc.; once you've sent them, set
the status to "Waiting" here until the company answers. "Declined" = you got an offer and said no.
"Company|part of position" targets one of several positions.
A company that isn't in your mails at all (offer by phone, applied via a
form without confirmation mail) gets added when you give an "applied" date:
  {"Some GmbH": {"position": "Werkstudent Backend", "status": "Declined",
                 "applied": "2026-07-01", "last": "2026-07-20"}}
"""
from __future__ import annotations

import argparse
import json
import re
import sqlite3
import subprocess
import sys
from datetime import date, datetime, timedelta
from difflib import SequenceMatcher
from pathlib import Path

GHOST_DAYS = 28        # waiting longer than this without any reply = "No reply"
QUIET_INTERVIEW = 14   # interview stage, quiet for this long = worth a follow-up

# ------------------------------------------------------------------ cleaning
NOISE_SENDERS = (
    "glassdoor.com", "jobagent.stepstone", "messages-noreply@linkedin",
    "updates-noreply@linkedin", "notifications-noreply@linkedin", "immobilienscout",
)
NOISE_SUBJECTS = (
    "this job is a match", "looking for candidates like you", "diese stelle würde",
    "bewerben sie sich noch heute", "passwort", "password", "security code",
    "sicherheitscode", "account created", "account has been created",
    "zugang zu unserem karriereportal", "deine anmeldung bei", "welcome to",
    "neues jobangebot", "möchten sie mehr über die stelle",
)
INTERVIEW_HINTS = ("interview", "kennenlernen", "videocall", "terminbestätigung",
                   "einladung zu einem ersten austausch", "vorstellungsgespräch")
TALENT_POOL_HINTS = ("talentpool", "talent pool", "talent-pool")
# the company is waiting on YOU (missing documents, unfinished application, ...)
ACTION_HINTS = ("vervollständig", "dokumente fehlen", "unterlagen fehlen", "ergänzung der bewerbungsunterlagen",
                "weitere unterlagen", "unterlagen erwartet", "nachreichen", "missing document",
                "additional document", "complete your application", "action required")

def _load_companies() -> dict:
    """Your own company lists live in companies.json next to this script (git-ignored,
    see companies.example.json). Missing file = empty lists, the tool still works."""
    path = Path(__file__).with_name("companies.json")
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except FileNotFoundError:
        return {}
    except json.JSONDecodeError as err:
        sys.exit(f"companies.json is not valid JSON: {err}")


_COMPANIES = _load_companies()
# sender domain -> company (beats whatever name the model read from the mail)
DOMAIN_COMPANY = _COMPANIES.get("domains", {})
# normalized model name (or its first words) -> one display name
ALIASES = {"indeed": "Unknown (via Indeed)", **_COMPANIES.get("aliases", {})}
LEGAL = {"gmbh", "ag", "se", "kg", "kgaa", "co", "mbh", "ug", "inc", "ltd", "group",
         "gruppe", "zentrale", "&", "germany", "deutschland"}

DECISIVE = {"rejection", "offer"}
EMAIL_RE = re.compile(r"[\w.+-]+@[\w-]+\.[\w.-]+")


def is_noise(sender: str, subject: str) -> bool:
    s, subj = sender.lower(), subject.lower()
    if any(n in s for n in NOISE_SENDERS) or any(n in subj for n in NOISE_SUBJECTS):
        return True
    # Indeed: keep only "X hat Sie zur Bewerbung eingeladen" (a real invite)
    return "no-reply@indeed.com" in s and "zur bewerbung eingeladen" not in subj


def norm_company(name: str) -> str:
    words = re.sub(r"[,()]", " ", name.lower().replace("​", "")).split()
    return " ".join(w for w in words if w.strip(".&") not in LEGAL and w != "&")


def pretty_company(name: str) -> str:
    words = name.replace("​", "").split()
    kept = [w for w in words if w.lower().strip(".,&") not in LEGAL and w != "&"]
    return " ".join(kept).strip(" ,") or name.strip() or "Unknown"


def canon_company(sender: str, company: str) -> str:
    addrs = EMAIL_RE.findall(sender.lower())
    dom = addrs[-1].split("@", 1)[1] if addrs else ""
    for d, name in DOMAIN_COMPANY.items():
        if dom == d or dom.endswith("." + d):
            return name
    key = norm_company(company or "")
    for k, v in ALIASES.items():
        if key == k or key.startswith(k + " "):
            return v
    return pretty_company(company or "Unknown")


def norm_position(p: str) -> str:
    p = p.lower().replace("​", "")
    p = re.sub(r"\((?:[^()]*?/){1,}[^()]*?\)", " ", p)        # (m/w/d), (w/m/d/x) ...
    p = re.sub(r"\b[dfmwx](?:/[dfmwx]){1,3}\b", " ", p)       # d/f/m without brackets
    p = re.sub(r"\(?\b(?:r-)?\d{5,}\b\)?", " ", p)            # job ids
    p = re.sub(r"werkstudent(?::in|\*in|/-in|/in|in)?|working student", "ws ", p)
    return re.sub(r"[^a-z0-9äöüß]+", " ", p).strip()


def similar(a: str, b: str) -> float:
    return SequenceMatcher(None, norm_position(a), norm_position(b)).ratio()


def clean_subject(s: str) -> str:
    s = EMAIL_RE.sub("", s.replace("​", ""))
    return re.sub(r"\(\s*\)", "", s).strip()


# ------------------------------------------------------------------ grouping
def load_events(db: sqlite3.Connection):
    rows = db.execute(
        "SELECT date, sender, subject, company, position, type, summary "
        "FROM mails WHERE is_job=1 ORDER BY date, rowid").fetchall()
    kept, dropped = [], 0
    for d, sender, subject, company, position, typ, summary in rows:
        sender, subject = sender or "", subject or ""
        if is_noise(sender, subject):
            dropped += 1
            continue
        text = f"{subject} {summary or ''}".lower()
        if typ in ("application_confirmation", "other") and any(h in subject.lower() for h in INTERVIEW_HINTS):
            typ = "interview_invite"
        job_id = re.search(r"\b(\d{5,7})\b", subject)
        kept.append({
            "date": d or "", "company": canon_company(sender, company or ""),
            "position": (position or "").replace("​", "").strip(),
            "type": typ or "other", "subject": clean_subject(subject),
            "summary": (summary or "").strip(), "job_id": job_id.group(1) if job_id else "",
            "talent_pool": any(h in text for h in TALENT_POOL_HINTS),
            "action": typ not in DECISIVE and any(h in text for h in ACTION_HINTS),
        })
    return kept, dropped, len(rows)


def group(events):
    apps = []
    for e in events:
        same = [a for a in apps if a["company"] == e["company"]]
        match = None
        if e["job_id"]:
            match = next((a for a in same if a["job_id"] == e["job_id"]), None)
            if match is None:
                match = next((a for a in same if not a["job_id"] and a["position"]
                              and similar(a["position"], e["position"]) >= 0.75), None)
        elif e["position"]:
            scored = [(similar(a["position"], e["position"]), a) for a in same if a["position"]]
            best = max(scored, key=lambda t: t[0], default=(0, None))
            match = best[1] if best[0] >= 0.75 else None
            if match is None:
                match = next((a for a in reversed(same) if not a["position"]), None)
        elif same:
            match = max(same, key=lambda a: a["events"][-1]["date"])

        # a new confirmation after a rejection = you applied again
        if match and e["type"] == "application_confirmation" and match["events"]:
            last_dec = [x for x in match["events"] if x["type"] in DECISIVE]
            if last_dec and e["date"] > last_dec[-1]["date"]:
                match = None
        if match is None:
            match = {"company": e["company"], "position": e["position"],
                     "job_id": e["job_id"], "events": []}
            apps.append(match)
        if not match["position"] and e["position"]:
            match["position"] = e["position"]
        if not match["job_id"] and e["job_id"]:
            match["job_id"] = e["job_id"]
        match["events"].append(e)
    return apps


def derive(app, today: date):
    ev = app["events"]
    types = [e["type"] for e in ev]
    dated = [e["date"] for e in ev if e["date"]]
    first, last = (min(dated), max(dated)) if dated else ("", "")
    quiet = (today - date.fromisoformat(last)).days if last else 0
    applied = any(t in ("application_confirmation", "interview_invite", "rejection", "offer")
                  for t in types)
    decisive = [e for e in ev if e["type"] in DECISIVE]
    if "offer" in types:
        status = "Offer"
    elif decisive and decisive[-1]["type"] == "rejection":
        status = "Rejected"
    elif any(e["talent_pool"] for e in ev):
        status = "Talent pool"
    elif "interview_invite" in types:
        status = "Interview"
    else:
        status = "Waiting" if quiet <= GHOST_DAYS else "No reply"
    # newest mail asks you for something and nothing has happened since
    if status in ("Waiting", "No reply") and ev and ev[-1]["action"]:
        status = "Action needed"
    responded = status in ("Offer", "Rejected", "Talent pool", "Interview")
    return {
        "company": app["company"], "position": app["position"],
        "status": status, "applied": first, "last": last, "quiet": quiet,
        "is_application": applied, "responded": responded,
        "interview": "interview_invite" in types, "offer": "offer" in types,
        "note": "",
        "events": [{k: e[k] for k in ("date", "type", "subject", "summary")} for e in ev],
    }


def apply_overrides(apps, path: Path, today: date):
    if not path.exists():
        return
    try:
        ov = json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError as err:
        print(f"overrides.json ignored: {err}")
        return
    for key, fields in ov.items():
        comp, _, pos_part = key.partition("|")
        hits = [a for a in apps if a["company"].lower() == comp.strip().lower()
                and (not pos_part or pos_part.strip().lower() in a["position"].lower())]
        if not hits and fields.get("applied"):  # not in the mails at all: add it
            last = fields.get("last", fields["applied"])
            new = {"company": comp.strip(), "position": fields.get("position", ""),
                   "status": "Waiting", "applied": fields["applied"], "last": last,
                   "quiet": (today - date.fromisoformat(last)).days, "is_application": True,
                   "responded": False, "interview": False, "offer": False, "note": "",
                   "events": [{"date": fields["applied"], "type": "other",
                               "subject": "Added by hand in overrides.json", "summary": ""}]}
            apps.append(new)
            hits = [new]
        for a in hits:
            for f in ("position", "status", "note"):
                if f in fields:
                    a[f] = fields[f]
            if a["status"] in ("Interview", "Offer", "Declined"):
                a["interview"] = True
            if a["status"] in ("Interview", "Offer", "Declined", "Rejected", "Talent pool"):
                a["responded"] = True
            if a["status"] in ("Offer", "Declined"):
                a["offer"] = True


# ------------------------------------------------------------------ build
def tracker(db_path: Path, today: date | None = None):
    """Cleaned applications (overrides applied, newest first) + stats. Used by
    build() and by jobmail.py for applications.csv/.xlsx."""
    today = today or date.today()
    db = sqlite3.connect(db_path)
    events, dropped, total_job = load_events(db)
    scanned = db.execute("SELECT COUNT(*) FROM mails").fetchone()[0]
    first_mail = db.execute("SELECT MIN(date) FROM mails WHERE date != ''").fetchone()[0] or ""
    db.close()

    apps = [derive(a, today) for a in group(events)]
    apply_overrides(apps, db_path.parent / "overrides.json", today)
    for a in apps:  # got as far as an interview, then a no
        a["after_interview"] = a["status"] == "Rejected" and a["interview"]
    apps.sort(key=lambda a: a["last"], reverse=True)
    return apps, {"scanned": scanned, "classified": total_job, "dropped": dropped,
                  "since": first_mail}


def build(db_path: Path, out_path: Path, today: date | None = None) -> Path:
    today = today or date.today()
    apps, st = tracker(db_path, today)
    scanned, total_job, dropped, first_mail = st["scanned"], st["classified"], st["dropped"], st["since"]

    data = {
        "today": today.isoformat(), "since": first_mail, "scanned": scanned,
        "classified": total_job, "dropped": dropped,
        "ghost_days": GHOST_DAYS, "quiet_interview": QUIET_INTERVIEW,
        "generated": datetime.now().strftime("%Y-%m-%d %H:%M"),
        "apps": [a for a in apps if a["is_application"]],
        "contacts": [a for a in apps if not a["is_application"]],
    }
    payload = json.dumps(data, ensure_ascii=False).replace("</", "<\\/")
    out_path.write_text(TEMPLATE.replace("__DATA__", payload), encoding="utf-8")
    return out_path


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", default=".", help="folder with jobmail.db (dashboard.html goes there too)")
    ap.add_argument("--open", action="store_true", help="open the dashboard afterwards")
    ap.add_argument("--today", help=argparse.SUPPRESS)
    args = ap.parse_args()
    folder = Path(args.out).expanduser()
    db_path = folder / "jobmail.db"
    if not db_path.exists():
        sys.exit(f"No jobmail.db in {folder.resolve()}. Run jobmail.py first.")
    today = date.fromisoformat(args.today) if args.today else None
    out = build(db_path, folder / "dashboard.html", today)
    print(f"Dashboard: {out.resolve()}")
    if args.open:
        opener = "open" if sys.platform == "darwin" else "xdg-open"
        subprocess.run([opener, str(out)], check=False)


# ------------------------------------------------------------------ page
TEMPLATE = r"""<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Application Tracker</title>
<style>
:root{
  color-scheme:light;
  --bg:#f4f3f0; --surface:#fcfcfb; --surface-2:#f0efec; --border:#e2e0da;
  --text:#0b0b0b; --text-2:#52514e; --muted:#7a7873;
  --series:#2a78d6; --series-soft:#cde2fb; --grid:#e8e6e1;
  --good:#0ca30c; --good-text:#006300; --warning:#fab219; --warning-text:#8a5a00;
  --critical:#d03b3b; --critical-text:#a12626; --neutral:#8a8882;
  --focus:#2a78d6;
}
@media (prefers-color-scheme: dark){
  :root:not([data-theme="light"]){
    color-scheme:dark;
    --bg:#121211; --surface:#1a1a19; --surface-2:#232321; --border:#33332f;
    --text:#f0efec; --text-2:#c3c2b7; --muted:#9a998f;
    --series:#3987e5; --series-soft:#1c3554; --grid:#2c2c29;
    --good-text:#5fd35f; --warning-text:#fab219; --critical-text:#ef7b7b; --neutral:#8f8e86;
  }
}
:root[data-theme="dark"]{
  color-scheme:dark;
  --bg:#121211; --surface:#1a1a19; --surface-2:#232321; --border:#33332f;
  --text:#f0efec; --text-2:#c3c2b7; --muted:#9a998f;
  --series:#3987e5; --series-soft:#1c3554; --grid:#2c2c29;
  --good-text:#5fd35f; --warning-text:#fab219; --critical-text:#ef7b7b; --neutral:#8f8e86;
}
*{box-sizing:border-box}
html,body{margin:0}
body{background:var(--bg);color:var(--text);font:14px/1.45 -apple-system,BlinkMacSystemFont,"SF Pro Text","Segoe UI",system-ui,sans-serif;-webkit-font-smoothing:antialiased}
.wrap{max-width:1180px;margin:0 auto;padding:28px 16px 48px}
header{display:flex;align-items:flex-end;justify-content:space-between;gap:16px;flex-wrap:wrap;margin-bottom:20px}
h1{font-size:24px;letter-spacing:-.01em;margin:0}
.sub{color:var(--text-2);margin-top:4px}
.theme{border:1px solid var(--border);background:var(--surface);color:var(--text-2);border-radius:8px;padding:6px 10px;font:inherit;cursor:pointer}
.kpis{display:grid;grid-template-columns:repeat(7,minmax(0,1fr));gap:12px;margin-bottom:16px}
.card{background:var(--surface);border:1px solid var(--border);border-radius:12px;padding:16px}
.kpi .label{color:var(--text-2);font-size:13px;display:flex;align-items:center;gap:6px}
.kpi .value{font-size:30px;font-weight:600;letter-spacing:-.02em;margin-top:6px;font-variant-numeric:tabular-nums}
.kpi .hint{color:var(--muted);font-size:12px;margin-top:2px}
.dot{width:8px;height:8px;border-radius:50%;display:inline-block;flex:none}
.grid2{display:grid;grid-template-columns:minmax(0,5fr) minmax(0,7fr);gap:16px;margin-bottom:16px}
h2{font-size:15px;margin:0 0 2px}
.card .desc{color:var(--text-2);font-size:13px;margin:0 0 14px}
.funnel .row{display:grid;grid-template-columns:118px 1fr 70px;align-items:center;gap:10px;margin:10px 0}
.funnel .name{color:var(--text-2)}
.funnel .track{height:22px;position:relative}
.funnel .bar{height:100%;background:var(--series);border-radius:0 4px 4px 0;min-width:2px}
.funnel .num{font-variant-numeric:tabular-nums;font-weight:600;text-align:right}
.funnel .num span{color:var(--muted);font-weight:400;font-size:12px;margin-left:4px}
#weekly{width:100%;height:220px;display:block}
#weekly text{fill:var(--text-2);font-size:11px}
#weekly .gl{stroke:var(--grid);stroke-width:1}
#weekly .bar{fill:var(--series)}
#weekly .hit{fill:transparent;cursor:default}
#weekly .hit:hover + .bar, #weekly g:hover .bar{opacity:.8}
details.tbl{margin-top:8px;color:var(--text-2);font-size:13px}
details.tbl table{margin-top:8px}
.tip{position:fixed;pointer-events:none;background:var(--text);color:var(--surface);padding:6px 9px;border-radius:6px;font-size:12px;line-height:1.35;opacity:0;transition:opacity .08s;z-index:10;white-space:nowrap}
.section{margin-bottom:16px}
table{width:100%;border-collapse:collapse}
th,td{text-align:left;padding:9px 10px;border-bottom:1px solid var(--border);vertical-align:top}
th{color:var(--text-2);font-weight:500;font-size:12px;white-space:nowrap;user-select:none}
th.sort{cursor:pointer}
th.sort:hover{color:var(--text)}
th .arr{opacity:.5;margin-left:3px}
td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
tr.app{cursor:pointer}
tr.app:hover td{background:var(--surface-2)}
tr.detail td{background:var(--surface-2);padding:6px 10px 12px}
.co{font-weight:600}
.pos{color:var(--text-2)}
.note{display:block;color:var(--muted);font-size:12px;margin-top:2px}
.pill{display:inline-flex;align-items:center;gap:6px;font-size:12px;font-weight:500;padding:2px 9px 2px 7px;border-radius:999px;border:1px solid var(--border);background:var(--surface);white-space:nowrap}
.pill svg{width:12px;height:12px;flex:none}
.s-Offer{color:var(--good-text)} .s-Interview{color:var(--series)} .s-Rejected{color:var(--critical-text)}
.flag{display:inline-block;margin-left:6px;font-size:11px;font-weight:500;padding:1px 7px;border-radius:999px;border:1px dashed var(--critical-text);color:var(--critical-text);white-space:nowrap;vertical-align:middle}
.s-No-reply{color:var(--warning-text)} .s-Action-needed{color:var(--warning-text);font-weight:600} .s-Declined{color:var(--good-text)} .s-Waiting{color:var(--text-2)} .s-Talent-pool{color:var(--text-2)}
.timeline{list-style:none;margin:0;padding:0}
.timeline li{display:grid;grid-template-columns:86px 150px 1fr;gap:10px;padding:5px 0;font-size:13px}
.timeline .d{color:var(--muted);font-variant-numeric:tabular-nums}
.timeline .t{color:var(--text-2)}
.timeline .sm{color:var(--muted);display:block}
.toolbar{display:flex;gap:10px;flex-wrap:wrap;align-items:center;margin-bottom:12px}
.toolbar input{flex:1 1 220px;min-width:0;padding:8px 11px;border:1px solid var(--border);border-radius:8px;background:var(--surface);color:var(--text);font:inherit}
.seg{display:flex;flex-wrap:wrap;gap:6px}
.seg button{border:1px solid var(--border);background:var(--surface);color:var(--text-2);border-radius:999px;padding:5px 11px;font:inherit;font-size:13px;cursor:pointer}
.seg button[aria-pressed="true"]{background:var(--text);color:var(--surface);border-color:var(--text)}
.empty{color:var(--muted);padding:14px 0}
.foot{color:var(--muted);font-size:12px;margin-top:20px}
.tablewrap{overflow-x:auto}
:focus-visible{outline:2px solid var(--focus);outline-offset:2px}
@media (max-width:1100px){.kpis{grid-template-columns:repeat(4,minmax(0,1fr))}}
@media (max-width:960px){.kpis{grid-template-columns:repeat(3,minmax(0,1fr))}.grid2{grid-template-columns:1fr}}
@media (max-width:560px){
  .kpis{grid-template-columns:repeat(2,minmax(0,1fr))}
  .kpi .value{font-size:26px}
  .hide-sm{display:none}
  .timeline li{grid-template-columns:78px 1fr}
  .timeline .t{grid-column:2}
  .timeline .s{grid-column:1/-1}
}
</style>
</head>
<body>
<div class="wrap">
  <header>
    <div>
      <h1>Application Tracker</h1>
      <div class="sub" id="sub"></div>
    </div>
    <button class="theme" id="theme" type="button">Theme: auto</button>
  </header>

  <div class="kpis" id="kpis"></div>

  <div class="grid2">
    <div class="card funnel">
      <h2>Funnel</h2>
      <p class="desc">How far applications got. Share of all applications.</p>
      <div id="funnel"></div>
    </div>
    <div class="card">
      <h2>Applications per week</h2>
      <p class="desc">By the date of the first mail about each application.</p>
      <svg id="weekly" role="img" aria-label="Applications per week"></svg>
      <details class="tbl"><summary>Show as table</summary><div id="weeklyTable"></div></details>
    </div>
  </div>

  <div class="card section">
    <h2>Follow up</h2>
    <p class="desc" id="fuDesc"></p>
    <div class="tablewrap" id="followup"></div>
  </div>

  <div class="card section">
    <h2>All applications</h2>
    <p class="desc">Click a row for every mail behind it.</p>
    <div class="toolbar">
      <input id="q" type="search" placeholder="Search company or position" aria-label="Search">
      <div class="seg" id="filters"></div>
    </div>
    <div class="tablewrap"><table id="tbl"></table></div>
  </div>

  <div class="card section" id="contactsCard">
    <h2>Recruiter contacts</h2>
    <p class="desc">Outreach and talent-pool mails without an application of yours.</p>
    <div class="tablewrap" id="contacts"></div>
  </div>

  <div class="foot" id="foot"></div>
</div>
<div class="tip" id="tip" role="tooltip"></div>

<script>
const D = __DATA__;
const STATUSES = ["Action needed","Waiting","Interview","No reply","Rejected","Talent pool","Offer","Declined"];
const ICON = {
  "Action needed":'<svg viewBox="0 0 12 12"><circle cx="6" cy="6" r="6" fill="#fab219"/><path d="M6 2.8v4" stroke="#3a2a00" stroke-width="1.6" stroke-linecap="round"/><circle cx="6" cy="9.1" r=".9" fill="#3a2a00"/></svg>',
  "Offer":'<svg viewBox="0 0 12 12"><circle cx="6" cy="6" r="6" fill="#0ca30c"/><path d="M3.3 6.2l1.8 1.8 3.6-3.8" stroke="#fff" stroke-width="1.6" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  "Interview":'<svg viewBox="0 0 12 12"><circle cx="6" cy="6" r="6" fill="currentColor"/><circle cx="6" cy="6" r="2.2" fill="#fff"/></svg>',
  "Rejected":'<svg viewBox="0 0 12 12"><circle cx="6" cy="6" r="6" fill="#d03b3b"/><path d="M4 4l4 4M8 4l-4 4" stroke="#fff" stroke-width="1.6" stroke-linecap="round"/></svg>',
  "No reply":'<svg viewBox="0 0 12 12"><path d="M6 .6l5.6 10.2H.4z" fill="#fab219"/><path d="M6 4.3v3" stroke="#3a2a00" stroke-width="1.4" stroke-linecap="round"/><circle cx="6" cy="9" r=".8" fill="#3a2a00"/></svg>',
  "Waiting":'<svg viewBox="0 0 12 12"><circle cx="6" cy="6" r="5.2" fill="none" stroke="currentColor" stroke-width="1.4"/><path d="M6 3.2V6l1.9 1.3" stroke="currentColor" stroke-width="1.4" fill="none" stroke-linecap="round"/></svg>',
  "Declined":'<svg viewBox="0 0 12 12"><circle cx="6" cy="6" r="5.2" fill="none" stroke="#0ca30c" stroke-width="1.4"/><path d="M3.6 6.1l1.6 1.6 3.2-3.4" stroke="#0ca30c" stroke-width="1.4" fill="none" stroke-linecap="round" stroke-linejoin="round"/></svg>',
  "Talent pool":'<svg viewBox="0 0 12 12"><circle cx="6" cy="6" r="5.2" fill="none" stroke="currentColor" stroke-width="1.4" stroke-dasharray="2 1.6"/></svg>'
};
const TYPE = {application_confirmation:"Confirmation",interview_invite:"Interview",rejection:"Rejection",
  offer:"Offer",recruiter_outreach:"Recruiter outreach",other:"Other"};
const esc = s => String(s ?? "").replace(/[&<>"']/g, c => ({"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]));
const MON = ["Jan","Feb","Mar","Apr","May","Jun","Jul","Aug","Sep","Oct","Nov","Dec"];
const fmt = iso => { if(!iso) return "–"; const [y,m,d] = iso.split("-"); return `${+d} ${MON[+m-1]}`; };
const fmtY = iso => { if(!iso) return "–"; const [y,m,d] = iso.split("-"); return `${+d} ${MON[+m-1]} ${y}`; };
const pill = s => `<span class="pill s-${s.replace(/ /g,"-")}">${ICON[s]||""}${esc(s)}</span>`;
const pct = (n,d) => d ? Math.round(100*n/d) : 0;
const AFTER = "After interview";  /* filter for rejections that came after an interview */
const flag = a => a.after_interview ? `<span class="flag" title="You had an interview, then got rejected">after interview</span>` : "";
const statusCell = a => pill(a.status) + flag(a);

/* theme toggle: auto -> light -> dark */
(() => {
  const btn = document.getElementById("theme"), order = ["auto","light","dark"];
  let mode = "auto";
  try { mode = localStorage.getItem("jt-theme") || "auto"; } catch(e) {}
  const set = m => { mode = m; if (m === "auto") document.documentElement.removeAttribute("data-theme");
    else document.documentElement.setAttribute("data-theme", m);
    btn.textContent = "Theme: " + m; try { localStorage.setItem("jt-theme", m); } catch(e) {} };
  set(mode);
  btn.onclick = () => { set(order[(order.indexOf(mode)+1) % 3]); drawWeekly(); };
})();

const A = D.apps;
const n = A.length;
const cnt = s => A.filter(a => a.status === s).length;
const responded = A.filter(a => a.responded).length;
const interviews = A.filter(a => a.interview).length;
const offers = A.filter(a => a.offer).length;
const declined = cnt("Declined");
const afterInterview = A.filter(a => a.after_interview).length;

document.getElementById("sub").textContent =
  `Mails since ${fmtY(D.since)} · updated ${D.generated}`;

/* KPI tiles */
const kpis = [
  {label:"Applications", value:n, hint:`${A.filter(a=>a.applied>=addDays(D.today,-30)).length} in the last 30 days`},
  {label:"Response rate", value:pct(responded,n)+"%", hint:`${responded} of ${n} heard back`},
  {label:"Interviews", value:interviews, hint:`${pct(interviews,n)}% of applications`, dot:"var(--series)"},
  {label:"Offers", value:offers, hint:declined?`${declined} declined by you`:`${pct(offers,interviews)}% of interviews`, dot:"#0ca30c"},
  {label:"Waiting", value:cnt("Waiting"), hint:`under ${D.ghost_days} days, no answer yet`},
  {label:"No reply", value:cnt("No reply"), hint:`silent for ${D.ghost_days}+ days`, dot:"#fab219"},
  {label:"Rejected", value:cnt("Rejected"), hint:`${pct(cnt("Rejected"),n)}% of applications${afterInterview?` · ${afterInterview} after interview`:""}`, dot:"#d03b3b"},
];
document.getElementById("kpis").innerHTML = kpis.map(k =>
  `<div class="card kpi"><div class="label">${k.dot?`<span class="dot" style="background:${k.dot}"></span>`:""}${k.label}</div>
   <div class="value">${k.value}</div><div class="hint">${k.hint}</div></div>`).join("");

function addDays(iso, d){ const t = new Date(iso+"T00:00:00Z"); t.setUTCDate(t.getUTCDate()+d); return t.toISOString().slice(0,10); }

/* Funnel */
const stages = [["Applied",n],["Got an answer",responded],["Interview",interviews],["Offer",offers]];
document.getElementById("funnel").innerHTML = stages.map(([name,v]) =>
  `<div class="row"><div class="name">${name}</div>
   <div class="track"><div class="bar" style="width:${n?Math.max(100*v/n,0.4):0}%"></div></div>
   <div class="num">${v}<span>${pct(v,n)}%</span></div></div>`).join("");

/* Weekly bars */
function mondayOf(iso){ const t = new Date(iso+"T00:00:00Z"); const wd = (t.getUTCDay()+6)%7; t.setUTCDate(t.getUTCDate()-wd); return t.toISOString().slice(0,10); }
function isoWeek(iso){ const t = new Date(iso+"T00:00:00Z"); t.setUTCDate(t.getUTCDate()+3-((t.getUTCDay()+6)%7));
  const y = new Date(Date.UTC(t.getUTCFullYear(),0,4)); return 1+Math.round(((t-y)/864e5-3+((y.getUTCDay()+6)%7))/7); }
const weeks = [];
(() => {
  const dates = A.map(a => a.applied).filter(Boolean).sort();
  if (!dates.length) return;
  let w = mondayOf(D.since && D.since < dates[0] ? D.since : dates[0]);
  const end = mondayOf(D.today);
  while (w <= end) { weeks.push({start:w, n:0, names:[]}); w = addDays(w,7); }
  for (const a of A) { if (!a.applied) continue; const wk = weeks.find(x => x.start === mondayOf(a.applied)); if (wk) { wk.n++; wk.names.push(a.company); } }
})();
document.getElementById("weeklyTable").innerHTML = `<table><thead><tr><th>Week</th><th>Starts</th><th class="num">Applications</th></tr></thead><tbody>${
  weeks.map(w=>`<tr><td>W${isoWeek(w.start)}</td><td>${fmtY(w.start)}</td><td class="num">${w.n}</td></tr>`).join("")}</tbody></table>`;

const tip = document.getElementById("tip");
function showTip(html, ev){ tip.innerHTML = html; tip.style.opacity = 1; moveTip(ev); }
function moveTip(ev){ const r = tip.getBoundingClientRect(); let x = ev.clientX + 12, y = ev.clientY - r.height - 10;
  if (x + r.width > innerWidth - 8) x = ev.clientX - r.width - 12; if (y < 8) y = ev.clientY + 16; tip.style.left = x+"px"; tip.style.top = y+"px"; }
function hideTip(){ tip.style.opacity = 0; }

function drawWeekly(){
  const svg = document.getElementById("weekly");
  const W = svg.clientWidth || 600, H = 220, padL = 26, padR = 6, padT = 10, padB = 24;
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  if (!weeks.length) { svg.innerHTML = ""; return; }
  const max = Math.max(1, ...weeks.map(w => w.n));
  const step = max <= 5 ? 1 : max <= 12 ? 2 : 5, top = Math.ceil(max/step)*step;
  const ih = H - padT - padB, iw = W - padL - padR, bw = iw / weeks.length, gap = Math.min(6, bw*0.3);
  const y = v => padT + ih - ih * v / top;
  let s = "";
  for (let v = 0; v <= top; v += step) s += `<line class="gl" x1="${padL}" x2="${W-padR}" y1="${y(v)}" y2="${y(v)}"/><text x="${padL-6}" y="${y(v)+3.5}" text-anchor="end">${v}</text>`;
  const every = Math.ceil(weeks.length / Math.max(1, Math.floor(iw / 46)));
  weeks.forEach((w, i) => {
    const x = padL + i*bw + gap/2, bwi = Math.max(1, bw - gap), h = ih * w.n / top, r = Math.min(4, bwi/2, h);
    const yt = padT + ih - h;
    const path = w.n ? `M${x},${padT+ih} V${yt+r} Q${x},${yt} ${x+r},${yt} H${x+bwi-r} Q${x+bwi},${yt} ${x+bwi},${yt+r} V${padT+ih} Z` : "";
    s += `<g data-i="${i}"><rect class="hit" x="${padL+i*bw}" y="${padT}" width="${bw}" height="${ih}"/>${path?`<path class="bar" d="${path}"/>`:""}</g>`;
    if (i % every === 0) s += `<text x="${x+bwi/2}" y="${H-7}" text-anchor="middle">${fmt(w.start)}</text>`;
  });
  svg.innerHTML = s;
  svg.querySelectorAll("g[data-i]").forEach(g => {
    const w = weeks[+g.dataset.i];
    const uniq = [...new Set(w.names)];
    const html = `<b>W${isoWeek(w.start)} · ${fmt(w.start)} – ${fmt(addDays(w.start,6))}</b><br>${w.n} application${w.n===1?"":"s"}${uniq.length?"<br>"+esc(uniq.slice(0,5).join(", "))+(uniq.length>5?` +${uniq.length-5}`:""):""}`;
    g.addEventListener("mousemove", e => showTip(html, e));
    g.addEventListener("mouseleave", hideTip);
  });
}
drawWeekly();
addEventListener("resize", drawWeekly);

/* Follow up */
const fuRank = a => a.status === "Action needed" ? 0 : a.status === "Interview" ? 1 : 2;
const fu = A.filter(a => a.status === "Action needed" || a.status === "No reply" || (a.status === "Interview" && a.quiet > D.quiet_interview))
            .sort((a,b) => fuRank(a) - fuRank(b) || b.quiet - a.quiet);
document.getElementById("fuDesc").textContent =
  `Companies waiting on you first, then interviews quiet for ${D.quiet_interview}+ days, then applications with no answer for ${D.ghost_days}+ days.`;
document.getElementById("followup").innerHTML = fu.length ? `<table><thead><tr><th>Company</th><th class="hide-sm">Position</th><th>Status</th><th>Last mail</th><th class="num">Days quiet</th></tr></thead><tbody>${
  fu.map(a => `<tr><td class="co">${esc(a.company)}</td><td class="pos hide-sm">${esc(a.position||"–")}</td><td>${pill(a.status)}</td><td>${fmt(a.last)}</td><td class="num">${a.quiet}</td></tr>`).join("")
}</tbody></table>` : `<div class="empty">Nothing to chase right now.</div>`;

/* Main table */
let filter = "All", query = "", sortKey = "last", sortDir = -1, open = new Set();
const cols = [["company","Company"],["position","Position"],["status","Status"],["applied","First mail"],["last","Last mail"],["n","Mails"]];
const filtersEl = document.getElementById("filters");
function renderFilters(){
  const opts = ["All", ...STATUSES.filter(s => cnt(s)), ...(afterInterview ? [AFTER] : [])];
  const num = o => o === "All" ? n : o === AFTER ? afterInterview : cnt(o);
  filtersEl.innerHTML = opts.map(o => `<button type="button" aria-pressed="${o===filter}" data-f="${o}">${o} <span style="opacity:.6">${num(o)}</span></button>`).join("");
  filtersEl.querySelectorAll("button").forEach(b => b.onclick = () => { filter = b.dataset.f; renderFilters(); renderTable(); });
}
document.getElementById("q").addEventListener("input", e => { query = e.target.value.trim().toLowerCase(); renderTable(); });

function timeline(a){
  return `<ul class="timeline">${a.events.map(e => `<li><span class="d">${fmtY(e.date)}</span><span class="t">${esc(TYPE[e.type]||e.type)}</span><span class="s">${esc(e.subject)}${e.summary?`<span class="sm">${esc(e.summary)}</span>`:""}</span></li>`).join("")}</ul>`;
}
function renderTable(){
  const rows = A.map((a,i) => ({...a, i, n:a.events.length}))
    .filter(a => filter === "All" || (filter === AFTER ? a.after_interview : a.status === filter))
    .filter(a => !query || (a.company+" "+a.position).toLowerCase().includes(query))
    .sort((x,y) => { const a = x[sortKey], b = y[sortKey]; return (a > b ? 1 : a < b ? -1 : 0) * sortDir; });
  const head = `<thead><tr>${cols.map(([k,l]) => `<th class="sort${k==="n"?" num":""}${k==="position"||k==="applied"||k==="n"?" hide-sm":""}" data-k="${k}" aria-sort="${sortKey===k?(sortDir>0?"ascending":"descending"):"none"}">${l}<span class="arr">${sortKey===k?(sortDir>0?"▲":"▼"):""}</span></th>`).join("")}</tr></thead>`;
  const body = rows.length ? rows.map(a => `<tr class="app" data-i="${a.i}" tabindex="0" aria-expanded="${open.has(a.i)}">
      <td class="co">${esc(a.company)}${a.note?`<span class="note">${esc(a.note)}</span>`:""}</td>
      <td class="pos hide-sm">${esc(a.position||"–")}</td><td>${statusCell(a)}</td>
      <td class="hide-sm">${fmt(a.applied)}</td><td>${fmt(a.last)}</td><td class="num hide-sm">${a.n}</td></tr>
      ${open.has(a.i)?`<tr class="detail"><td colspan="6">${timeline(a)}</td></tr>`:""}`).join("")
    : `<tr><td colspan="6" class="empty">No applications match.</td></tr>`;
  const t = document.getElementById("tbl");
  t.innerHTML = head + `<tbody>${body}</tbody>`;
  t.querySelectorAll("th.sort").forEach(th => th.onclick = () => {
    const k = th.dataset.k; if (sortKey === k) sortDir *= -1; else { sortKey = k; sortDir = (k==="company"||k==="position"||k==="status") ? 1 : -1; } renderTable(); });
  t.querySelectorAll("tr.app").forEach(tr => {
    const toggle = () => { const i = +tr.dataset.i; open.has(i) ? open.delete(i) : open.add(i); renderTable(); };
    tr.onclick = toggle; tr.onkeydown = e => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); toggle(); } };
  });
}
renderFilters(); renderTable();

/* Contacts */
const C = D.contacts;
if (!C.length) document.getElementById("contactsCard").style.display = "none";
else document.getElementById("contacts").innerHTML = `<table><thead><tr><th>Company</th><th>Last mail</th><th>What</th></tr></thead><tbody>${
  C.map(c => { const e = c.events[c.events.length-1]; return `<tr><td class="co">${esc(c.company)}</td><td>${fmt(c.last)}</td><td class="pos">${esc(e.summary||e.subject)}</td></tr>`; }).join("")}</tbody></table>`;

document.getElementById("foot").textContent =
  `${D.scanned} mails scanned · ${D.classified} classified as job-related by the local model · ${D.dropped} of those dropped as job alerts or portal noise. ` +
  `Statuses are inferred from mails; fix anything wrong in overrides.json.`;
</script>
</body>
</html>
"""

if __name__ == "__main__":
    main()