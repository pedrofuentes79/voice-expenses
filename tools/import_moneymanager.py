"""Import expenses from a Money Manager (Realbyte, iOS) .mmbak backup into the app.

The .mmbak is the app's Core Data SQLite store. Categories and payment methods are mapped onto
the app's own through a JSON file (kept out of the repo: it names your categories):

    {
      "categories": {"Salidas/Con amigos": "Salidas", "Casa": "Hogar", ...},
      "payment_methods": {"Cash": "Cash", ...}
    }

Category keys are "Parent/Sub" or "Parent"; a transaction whose category isn't mapped is
skipped. An unmapped payment method is imported as null. Entry ids are derived from the backup's
row ids, so re-running the import updates instead of duplicating.

    python3 -I tools/import_moneymanager.py BACKUP.mmbak MAPPING.json --url https://host [--dry-run]
    python3 -I tools/import_moneymanager.py BACKUP.mmbak MAPPING.json --examples data/config.json
"""

import argparse
import json
import sqlite3
import sys
import urllib.request
import uuid
from collections import Counter, defaultdict
from datetime import UTC, date, datetime

NAMESPACE = uuid.UUID("2f1d7c55-4e0b-4b8e-9a51-6d1f0a3c9e77")
CURRENCIES = {"ARS_ARS": "ARS", "ARS_USD": "USD"}
EXAMPLES_PER_CATEGORY = 5

QUERY = """
SELECT t.ZUID, t.ZTXDATESTR, t.ZAMOUNT, t.ZAMOUNTSUB, t.ZCURRENCYUID, t.ZCONTENT, t.ZMEMO,
       c.ZNAME, p.ZNAME, a.ZNICNAME
FROM ZINOUTCOME t
LEFT JOIN ZCATEGORY c ON c.ZUID = t.ZCATEGORYUID
LEFT JOIN ZCATEGORY p ON p.ZUID = c.ZPUID
LEFT JOIN ZASSET a ON a.ZUID = t.ZASSETUID
WHERE t.ZISDEL = 0 AND t.ZDO_TYPE = '1'
ORDER BY t.ZTXDATESTR
"""


def load(backup: str, mapping: dict) -> tuple[list[dict], Counter]:
    db = sqlite3.connect(f"file:{backup}?mode=ro", uri=True)
    today = date.today().isoformat()
    entries, skipped = [], Counter()
    for uid, day, amount, amount_sub, currency_uid, content, memo, cat, parent, asset in db.execute(QUERY):
        key = f"{parent}/{cat}" if parent else cat
        if (category := mapping["categories"].get(key)) is None:
            skipped[f"category {key}"] += 1
            continue
        if (currency := CURRENCIES.get(currency_uid)) is None:
            skipped[f"currency {currency_uid}"] += 1
            continue
        if day > today:
            skipped["future-dated"] += 1
            continue
        # ZAMOUNT is converted to the main currency; ZAMOUNTSUB holds the original amount.
        value = amount_sub if currency != "ARS" and amount_sub else amount
        if not value or value <= 0:
            skipped["non-positive amount"] += 1
            continue
        note = " — ".join(s.strip() for s in (content, memo) if s and s.strip())
        entries.append({
            "id": str(uuid.uuid5(NAMESPACE, uid)),
            "status": "ok",
            "source": "import",
            "amount": round(value, 2),
            "currency": currency,
            "category": category,
            "merchant": None,
            "payment_method": mapping["payment_methods"].get(asset),
            "note": note,
            "date": day,
        })
    return entries, skipped


def upload(entries: list[dict], url: str) -> None:
    stamp = datetime.now(UTC).strftime("%Y-%m-%dT%H:%M:%S.000Z")
    for i, e in enumerate(entries, 1):
        req = urllib.request.Request(
            f"{url.rstrip('/')}/api/entries/{e['id']}",
            data=json.dumps({**e, "updated_at": stamp}).encode(),
            headers={"Content-Type": "application/json"},
            method="PUT",
        )
        with urllib.request.urlopen(req, timeout=15) as res:
            res.read()
        if i % 100 == 0:
            print(f"  {i}/{len(entries)}")


def write_examples(entries: list[dict], config_path: str) -> None:
    """Replace the config's imported examples with the latest distinct notes per category."""
    by_category: dict[str, list[dict]] = defaultdict(list)
    for e in sorted(entries, key=lambda e: e["date"], reverse=True):
        notes = {x["note"].lower() for x in by_category[e["category"]]}
        if e["note"] and e["note"].lower() not in notes and len(by_category[e["category"]]) < EXAMPLES_PER_CATEGORY:
            by_category[e["category"]].append(e)
    with open(config_path) as f:
        config = json.load(f)
    kept = [x for x in config.get("examples", []) if x.get("source") != "moneymanager"]
    imported = [
        {"note": e["note"], "kind": "expense", "amount": e["amount"], "currency": e["currency"],
         "category": e["category"], "payment_method": e["payment_method"] or "", "source": "moneymanager"}
        for group in by_category.values() for e in group
    ]
    config["examples"] = kept + imported
    with open(config_path, "w") as f:
        json.dump(config, f, ensure_ascii=False, indent=2)
        f.write("\n")
    print(f"examples: kept {len(kept)}, added {len(imported)} across {len(by_category)} categories")


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("backup")
    ap.add_argument("mapping")
    ap.add_argument("--url", help="server to PUT entries to")
    ap.add_argument("--examples", metavar="CONFIG_JSON", help="write prompt examples into this config")
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    with open(args.mapping) as f:
        mapping = json.load(f)
    entries, skipped = load(args.backup, mapping)
    print(f"{len(entries)} to import; skipped: {dict(skipped) or 'none'}")
    print("by category:", dict(Counter(e["category"] for e in entries).most_common()))
    print("by payment method:", dict(Counter(e["payment_method"] for e in entries).most_common()))
    if args.examples and not args.dry_run:
        write_examples(entries, args.examples)
    if args.url and not args.dry_run:
        upload(entries, args.url)
        print(f"uploaded {len(entries)}")
    elif not args.examples and not args.dry_run:
        sys.exit("nothing to do: pass --url and/or --examples")


if __name__ == "__main__":
    main()
