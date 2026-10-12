"""Extract categories, payment methods and example transactions from a Kesef backup into config.json."""

import json
import sys
from pathlib import Path

backup = json.loads(Path(sys.argv[1]).read_text())["tables"]
categories = {c["id"]: c for c in backup["categories"] if not c["deleted_at"]}
payment_methods = {p["id"]: p for p in backup["payment_methods"] if not p["deleted_at"]}


def names(rows: dict, **match: object) -> list[str]:
    selected = [r for r in rows.values() if all(r[k] == v for k, v in match.items())]
    return [r["name"] for r in sorted(selected, key=lambda r: r["sort_order"])]


out = Path(__file__).parent / "config.json"
previous = json.loads(out.read_text()) if out.exists() else {}
config = {
    "category_descriptions": previous.get("category_descriptions", {}),
    "expense_categories": names(categories, type="expense"),
    "income_categories": names(categories, type="income"),
    "payment_methods": names(payment_methods),
    "examples": [
        {
            "note": t["note"],
            "kind": t["type"],
            "amount": t["amount_cents"] / 100,
            "currency": t["currency_original"],
            "category": categories[t["category_id"]]["name"],
            "payment_method": payment_methods[t["payment_method_id"]]["name"],
        }
        for t in sorted(backup["transactions"], key=lambda t: t["date"], reverse=True)
        if t["category_id"] in categories and t["payment_method_id"] in payment_methods
    ][:50],
}
out.write_text(json.dumps(config, ensure_ascii=False, indent=2))
print(
    f"wrote {out}: {len(config['expense_categories'])} expense / {len(config['income_categories'])} income "
    f"categories, {len(config['payment_methods'])} payment methods, {len(config['examples'])} examples"
)
