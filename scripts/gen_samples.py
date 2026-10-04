#!/usr/bin/env python3
"""Generate the synthetic sample trade documents used to demo and test TradeDoc Check.

Writes four one-page A4 PDFs (a commercial invoice and a packing list for
sample A and sample B) to samples/ and copies them to web/public/samples/ so
the UI can offer them as downloads. Everything printed on them is fictional.

Sample A contains deliberate problems (a wrong invoice total, a quantity and
a material word that differ between the documents, a wrong carton total);
sample B is consistent, and its CIF invoice prints no freight or insurance row.
The numbers come from the "Synthetic sample documents" section of
docs/CONTRACT.md and are typed here exactly as they should be printed, so the
script never formats or computes a value that appears on a document.

After writing the PDFs the script extracts their text again and checks that
  1. every value from the contract appears exactly as printed, and
  2. every evidence quote in samples/expected/*.json is an exact substring of
     the page text, and the numeric values agree with their quotes.
It exits non-zero if either check fails.

Usage: python3 scripts/gen_samples.py   (needs reportlab; text checks use
pypdf if installed, otherwise poppler's pdftotext)
"""

import json
import re
import shutil
import subprocess
import sys
from decimal import Decimal
from pathlib import Path

from reportlab.lib import colors
from reportlab.lib.pagesizes import A4
from reportlab.pdfgen import canvas

ROOT = Path(__file__).resolve().parent.parent
SAMPLES = ROOT / "samples"
EXPECTED = SAMPLES / "expected"
WEB_COPY = ROOT / "web" / "public" / "samples"

PAGE_W, PAGE_H = A4
MARGIN = 40
CONTENT_W = PAGE_W - 2 * MARGIN
FOOTER = "SYNTHETIC SAMPLE - fictional companies, for software testing only"

# ---------------------------------------------------------------------------
# Document data. Strings are exactly what gets printed.
# ---------------------------------------------------------------------------

HARBOUR = {
    "name": "Harbour Ceramics Co., Ltd.",
    "address": [
        "No. 18 Example Industrial Road, Beilun District",
        "Ningbo, Zhejiang, China",
        "export@harbourceramics.example",
    ],
}

BLUE_GUM = {
    "name": "Blue Gum Textiles Co., Ltd.",
    "address": [
        "Lot 7, Example Export Processing Zone, District 7",
        "Ho Chi Minh City, Vietnam",
        "sales@bluegumtextiles.example",
    ],
}

SOUTHBANK = {
    "name": "Southbank Homewares Pty Ltd",
    "address": [
        "Unit 4, 21 Sample Street",
        "Southbank VIC 3006, Australia",
    ],
}

# Each document lists its header fields as (label, value) pairs; they are
# drawn as "Label: value" on one line so text extraction keeps them together.
DOCS = {
    "sample-a-invoice": {
        "kind": "invoice",
        "seller": HARBOUR,
        "buyer": SOUTHBANK,
        "fields": [
            ("Invoice No.", "INV-2026-0042"),
            ("Date", "15 Sep 2026"),
            ("Terms of delivery", "FOB Ningbo (Incoterms 2020)"),
            ("Currency", "USD"),
            ("Payment terms", "T/T 30 days after B/L date"),
            ("Port of loading", "Ningbo, China"),
            ("Port of discharge", "Melbourne, Australia"),
        ],
        # No., description, qty, unit, unit price, amount
        "lines": [
            ("1", "Porcelain dinner plate, 27 cm, white glazed", "1,200", "PCS", "3.50", "4,200.00"),
            ("2", "Stoneware coffee mug, 350 ml", "800", "PCS", "2.10", "1,680.00"),
            ("3", "Bamboo serving board, 40 x 25 cm", "300", "PCS", "4.80", "1,440.00"),
        ],
        # Deliberately wrong: the lines add up to 7,320.00 (digits transposed).
        "total": "7,302.00",
        "origin": "China",
    },
    "sample-a-packing-list": {
        "kind": "packing",
        "seller": HARBOUR,
        "buyer": SOUTHBANK,
        "fields": [
            ("Packing List No.", "PL-2026-0042"),
            ("Date", "15 Sep 2026"),
            ("Invoice No.", "INV-2026-0042"),
            ("Port of loading", "Ningbo, China"),
            ("Port of discharge", "Melbourne, Australia"),
        ],
        "marks": ["SOUTHBANK HOMEWARES", "MELBOURNE", "MADE IN CHINA", "C/NO. 1-UP"],
        # No., description, qty, unit, packages, gross weight (kg)
        "lines": [
            ("1", "Porcelain dinner plate, 27 cm", "1,200", "PCS", "50 CTNS", "540.00"),
            # Deliberate: 780 here vs 800 on the invoice, and "Ceramic" vs "Stoneware".
            ("2", "Ceramic coffee mug, 350 ml", "780", "PCS", "20 CTNS", "210.00"),
            ("3", "Bamboo serving board, 40 x 25 cm", "300", "PCS", "12 CTNS", "186.00"),
        ],
        # Deliberately wrong: the lines add up to 82 cartons. The weight is right.
        "total_packages": "84 CTNS",
        "total_weight": "936.00",
    },
    "sample-b-invoice": {
        "kind": "invoice",
        "seller": BLUE_GUM,
        "buyer": SOUTHBANK,
        "fields": [
            ("Invoice No.", "INV-2026-0057"),
            ("Date", "22 Sep 2026"),
            ("Terms of delivery", "CIF Melbourne (Incoterms 2020)"),
            ("Currency", "USD"),
            ("Payment terms", "T/T in advance"),
            ("Port of loading", "Ho Chi Minh City, Vietnam"),
            ("Port of discharge", "Melbourne, Australia"),
        ],
        "lines": [
            ("1", "100% cotton terry bath towel, 70 x 140 cm", "2,000", "PCS", "2.85", "5,700.00"),
            ("2", "Polyester fleece throw blanket, 127 x 152 cm", "500", "PCS", "4.40", "2,200.00"),
            ("3", "Cotton face cloth, 30 x 30 cm", "5,000", "PCS", "0.385", "1,925.00"),
        ],
        "total": "9,825.00",
        "origin": "Vietnam",
    },
    "sample-b-packing-list": {
        "kind": "packing",
        "seller": BLUE_GUM,
        "buyer": SOUTHBANK,
        "fields": [
            ("Packing List No.", "PL-2026-0057"),
            ("Date", "22 Sep 2026"),
            ("Invoice No.", "INV-2026-0057"),
            ("Port of loading", "Ho Chi Minh City, Vietnam"),
            ("Port of discharge", "Melbourne, Australia"),
        ],
        "marks": ["SOUTHBANK HOMEWARES", "MELBOURNE", "MADE IN VIETNAM", "C/NO. 1-UP"],
        "lines": [
            ("1", "100% cotton terry bath towel, 70 x 140 cm", "2,000", "PCS", "40 CTNS", "620.00"),
            ("2", "Polyester fleece throw blanket, 127 x 152 cm", "500", "PCS", "25 CTNS", "310.00"),
            ("3", "Cotton face cloth, 30 x 30 cm", "5,000", "PCS", "20 CTNS", "260.00"),
        ],
        "total_packages": "85 CTNS",
        "total_weight": "1,190.00",
    },
}

# What the contract says must be printed, copied from docs/CONTRACT.md rather
# than from DOCS above, so a typo in DOCS is caught. "absent" lists the correct
# sums that must NOT be printed: sample A shows the wrong totals on purpose.
SPEC = {
    "sample-a-invoice": {
        "present": [
            "INV-2026-0042", "15 Sep 2026", "Harbour Ceramics Co., Ltd.", "Ningbo",
            "Southbank Homewares Pty Ltd", "Southbank VIC 3006, Australia", "USD", "FOB Ningbo",
            "Country of origin: China",
            "Porcelain dinner plate, 27 cm, white glazed", "1,200", "3.50", "4,200.00",
            "Stoneware coffee mug, 350 ml", "800", "2.10", "1,680.00",
            "Bamboo serving board, 40 x 25 cm", "300", "4.80", "1,440.00",
            "7,302.00", FOOTER,
        ],
        "absent": ["7,320.00"],
    },
    "sample-a-packing-list": {
        "present": [
            "PL-2026-0042", "Invoice No.: INV-2026-0042", "Harbour Ceramics Co., Ltd.",
            "Southbank Homewares Pty Ltd",
            "Porcelain dinner plate, 27 cm", "1,200", "50 CTNS", "540.00",
            "Ceramic coffee mug, 350 ml", "780", "20 CTNS", "210.00",
            "Bamboo serving board, 40 x 25 cm", "300", "12 CTNS", "186.00",
            "84 CTNS", "936.00", FOOTER,
        ],
        "absent": ["82 CTNS", "800"],
    },
    "sample-b-invoice": {
        "present": [
            "INV-2026-0057", "Blue Gum Textiles Co., Ltd.", "Ho Chi Minh City, Vietnam",
            "Southbank Homewares Pty Ltd", "USD", "CIF Melbourne", "Country of origin: Vietnam",
            "100% cotton terry bath towel, 70 x 140 cm", "2,000", "2.85", "5,700.00",
            "Polyester fleece throw blanket, 127 x 152 cm", "500", "4.40", "2,200.00",
            "Cotton face cloth, 30 x 30 cm", "5,000", "0.385", "1,925.00",
            "9,825.00", FOOTER,
        ],
        # A CIF invoice with no freight or insurance row is what makes the
        # checks report freight_insurance_not_shown; the fixture's empty
        # "charges" list depends on these words not being printed.
        "absent": ["Freight", "Insurance"],
    },
    "sample-b-packing-list": {
        "present": [
            "PL-2026-0057", "Invoice No.: INV-2026-0057", "Blue Gum Textiles Co., Ltd.",
            "Southbank Homewares Pty Ltd",
            "100% cotton terry bath towel, 70 x 140 cm", "2,000", "40 CTNS", "620.00",
            "Polyester fleece throw blanket, 127 x 152 cm", "500", "25 CTNS", "310.00",
            "Cotton face cloth, 30 x 30 cm", "5,000", "20 CTNS", "260.00",
            "85 CTNS", "1,190.00", FOOTER,
        ],
        "absent": [],
    },
}

# ---------------------------------------------------------------------------
# Drawing helpers. Every piece of text is a real text string in a built-in
# font (never an image), so a model or pdftotext can read it back exactly.
# ---------------------------------------------------------------------------

GRID = colors.HexColor("#555555")
HEADER_FILL = colors.HexColor("#E6E9EE")
MUTED = colors.HexColor("#666666")


def draw_label_value(c, x, y, label, value, size=9):
    """Draw 'Label: value' as one line with a bold label.

    Both parts go into one text object, separated by a real space character,
    so extracted text reads "Invoice No.: INV-2026-0042" as a single run.
    """
    t = c.beginText(x, y)
    t.setFont("Helvetica-Bold", size)
    t.textOut(label + ": ")
    t.setFont("Helvetica", size)
    t.textOut(value)
    c.drawText(t)


def draw_party(c, x, y, heading, party):
    """Draw a seller/buyer block: small heading, bold name, address lines."""
    c.setFont("Helvetica-Bold", 7.5)
    c.setFillColor(MUTED)
    c.drawString(x, y, heading.upper())
    c.setFillColor(colors.black)
    c.setFont("Helvetica-Bold", 10)
    c.drawString(x, y - 14, party["name"])
    c.setFont("Helvetica", 9)
    for i, line in enumerate(party["address"]):
        c.drawString(x, y - 27 - i * 11.5, line)


def draw_letterhead(c, party, title):
    """Company name and address at the top, then the document title."""
    top = PAGE_H - MARGIN
    c.setFont("Helvetica-Bold", 15)
    c.drawString(MARGIN, top - 12, party["name"].upper())
    c.setFont("Helvetica", 8.5)
    c.setFillColor(MUTED)
    c.drawString(MARGIN, top - 26, "   |   ".join(party["address"]))
    c.setFillColor(colors.black)
    c.setStrokeColor(colors.black)
    c.setLineWidth(1.2)
    c.line(MARGIN, top - 34, PAGE_W - MARGIN, top - 34)
    c.setFont("Helvetica-Bold", 16)
    c.drawCentredString(PAGE_W / 2, top - 62, title)
    return top - 80  # y where the next section starts


def draw_header_box(c, y, doc, left_headings):
    """Bordered box: parties on the left, document reference fields on the right."""
    height = 150
    mid = MARGIN + CONTENT_W * 0.52
    c.setStrokeColor(GRID)
    c.setLineWidth(0.6)
    c.rect(MARGIN, y - height, CONTENT_W, height)
    c.line(mid, y, mid, y - height)
    c.line(MARGIN, y - height / 2, mid, y - height / 2)

    draw_party(c, MARGIN + 8, y - 14, left_headings[0], doc["seller"])
    draw_party(c, MARGIN + 8, y - height / 2 - 14, left_headings[1], doc["buyer"])

    for i, (label, value) in enumerate(doc["fields"]):
        draw_label_value(c, mid + 10, y - 20 - i * 18, label, value)
    return y - height - 18


def draw_table(c, y, headers, widths, numeric, rows, total_row):
    """Draw a ruled table. numeric[i] right-aligns column i (numbers line up).

    total_row is a list with the same number of cells as headers; empty
    strings leave a cell blank. It is drawn in bold under a heavier rule.
    """
    row_h = 20
    header_h = 24
    xs = [MARGIN]
    for w in widths:
        xs.append(xs[-1] + w)

    def cell(text, col, baseline, font):
        c.setFont(font, 9)
        if numeric[col]:
            c.drawRightString(xs[col + 1] - 6, baseline, text)
        else:
            c.drawString(xs[col] + 6, baseline, text)

    # Header row with a light fill.
    c.setFillColor(HEADER_FILL)
    c.rect(MARGIN, y - header_h, CONTENT_W, header_h, stroke=0, fill=1)
    c.setFillColor(colors.black)
    for col, h in enumerate(headers):
        c.setFont("Helvetica-Bold", 8.5)
        if numeric[col]:
            c.drawRightString(xs[col + 1] - 6, y - 15, h)
        else:
            c.drawString(xs[col] + 6, y - 15, h)

    cur = y - header_h
    for row in rows:
        for col, text in enumerate(row):
            cell(text, col, cur - 13.5, "Helvetica")
        cur -= row_h
    body_bottom = cur
    for col, text in enumerate(total_row):
        if text:
            cell(text, col, cur - 13.5, "Helvetica-Bold")
    bottom = cur - row_h

    # Grid: outer box, column lines, a rule under the header and above the total.
    c.setStrokeColor(GRID)
    c.setLineWidth(0.6)
    c.rect(MARGIN, bottom, CONTENT_W, y - bottom)
    for x in xs[1:-1]:
        c.line(x, y, x, bottom)
    c.line(MARGIN, y - header_h, PAGE_W - MARGIN, y - header_h)
    c.setLineWidth(1.1)
    c.line(MARGIN, body_bottom, PAGE_W - MARGIN, body_bottom)
    return bottom - 22


def draw_signature(c, y, party):
    x = PAGE_W - MARGIN - 210
    c.setFont("Helvetica", 9)
    c.drawString(x, y, "For and on behalf of")
    c.setFont("Helvetica-Bold", 9)
    c.drawString(x, y - 12, party["name"])
    c.setStrokeColor(colors.black)
    c.setLineWidth(0.6)
    c.line(x, y - 52, PAGE_W - MARGIN, y - 52)
    c.setFont("Helvetica", 8)
    c.setFillColor(MUTED)
    c.drawString(x, y - 63, "Authorised signature")
    c.setFillColor(colors.black)


def draw_footer(c):
    c.setFont("Helvetica-Oblique", 7.5)
    c.setFillColor(MUTED)
    c.drawCentredString(PAGE_W / 2, MARGIN - 14, FOOTER)
    c.setFillColor(colors.black)


TABLE_WIDTHS = [30, 213, 55, 40, 80, CONTENT_W - 418]


def draw_invoice(c, doc):
    y = draw_letterhead(c, doc["seller"], "COMMERCIAL INVOICE")
    y = draw_header_box(c, y, doc, ("Seller / Exporter", "Buyer / Consignee"))
    total = ["", "TOTAL", "", "", "USD", doc["total"]]
    y = draw_table(
        c, y,
        ["No.", "Description of goods", "Qty", "Unit", "Unit price (USD)", "Amount (USD)"],
        TABLE_WIDTHS,
        [False, False, True, False, True, True],
        doc["lines"], total,
    )
    c.setFont("Helvetica", 9)
    c.drawString(MARGIN, y, "Country of origin: " + doc["origin"])
    c.drawString(MARGIN, y - 22, "We certify that this invoice shows the actual price of the goods described")
    c.drawString(MARGIN, y - 34, "and that all particulars are true and correct.")
    draw_signature(c, y - 70, doc["seller"])
    draw_footer(c)


def draw_packing_list(c, doc):
    y = draw_letterhead(c, doc["seller"], "PACKING LIST")
    y = draw_header_box(c, y, doc, ("Shipper", "Consignee"))
    total = ["", "TOTAL", "", "", doc["total_packages"], doc["total_weight"]]
    y = draw_table(
        c, y,
        ["No.", "Description of goods", "Qty", "Unit", "Packages", "Gross weight (kg)"],
        TABLE_WIDTHS,
        [False, False, True, False, True, True],
        doc["lines"], total,
    )
    c.setFont("Helvetica-Bold", 9)
    c.drawString(MARGIN, y, "Marks & numbers:")
    c.setFont("Helvetica", 9)
    for i, mark in enumerate(doc["marks"]):
        c.drawString(MARGIN, y - 14 - i * 12, mark)
    c.drawString(MARGIN, y - 80, "Packed in export cartons.")
    draw_signature(c, y - 110, doc["seller"])
    draw_footer(c)


def write_pdf(name, doc):
    path = SAMPLES / f"{name}.pdf"
    # invariant=1 fixes the creation date and document ID that reportlab would
    # otherwise stamp into the file, so re-running produces identical bytes.
    c = canvas.Canvas(str(path), pagesize=A4, invariant=1)
    c.setTitle(name)
    c.setAuthor("TradeDoc Check sample generator")
    c.setSubject(FOOTER)
    c.setCreator("scripts/gen_samples.py")
    if doc["kind"] == "invoice":
        draw_invoice(c, doc)
    else:
        draw_packing_list(c, doc)
    c.showPage()
    c.save()
    return path


# ---------------------------------------------------------------------------
# Verification: read the text back out of each PDF.
# ---------------------------------------------------------------------------


def extract_texts(path):
    """Return one or more extracted page-1 texts, or None if no extractor exists.

    pdftotext is run in its default, -raw and -layout modes: a quote counts
    only if it survives all three, because we can't know exactly how a model
    or another tool will lay out the text.
    """
    try:
        from pypdf import PdfReader

        return {"pypdf": PdfReader(str(path)).pages[0].extract_text()}
    except ImportError:
        pass
    if shutil.which("pdftotext") is None:
        return None
    out = {}
    for mode in ([], ["-raw"], ["-layout"]):
        res = subprocess.run(
            ["pdftotext", "-f", "1", "-l", "1", "-enc", "UTF-8", *mode, str(path), "-"],
            check=True, capture_output=True, text=True,
        )
        out["pdftotext " + (mode[0] if mode else "default")] = res.stdout
    return out


def number_in(quote):
    """The first number printed in a quote without its thousands separators,
    e.g. '1,200 PCS' -> '1200', or None if the quote has no number."""
    m = re.search(r"\d[\d,]*(\.\d+)?", quote)
    return m.group(0).replace(",", "") if m else None


# Decimal-string fields hold the printed number exactly, minus thousands
# separators: "1,200" -> "1200", "0.385" -> "0.385" (never "1200.0" or "3.5").
DECIMAL_STRINGS = {"quantity", "unit_price"}

# Integer fields hold the printed number scaled to minor units or grams.
SCALE = {
    "packages": 1, "total_packages": 1,
    "line_total_minor": 100, "total_minor": 100, "amount_minor": 100,
    "gross_weight_grams": 1000, "total_gross_weight_grams": 1000,
}


def check_fields(obj, path, texts, problems):
    """Walk an expected payload, check every Field's quote and value, and
    return how many quotes were checked."""
    if isinstance(obj, list):
        return sum(check_fields(item, f"{path}[{i}]", texts, problems) for i, item in enumerate(obj))
    if not isinstance(obj, dict):
        return 0
    if "evidence" in obj and obj.get("value") is not None:
        quote = obj["evidence"]["quote"]
        for mode, text in texts.items():
            if quote not in text:
                problems.append(f"{path}: quote {quote!r} not found ({mode})")
        key = path.rsplit(".", 1)[-1]
        printed = number_in(quote)
        if key in DECIMAL_STRINGS and printed != obj["value"]:
            problems.append(f"{path}: value {obj['value']!r} is not the number printed in {quote!r}")
        if key in SCALE and (printed is None or Decimal(printed) * SCALE[key] != obj["value"]):
            problems.append(f"{path}: value {obj['value']} does not match quote {quote!r}")
        return 1
    return sum(check_fields(v, f"{path}.{k}" if path else k, texts, problems) for k, v in obj.items())


def verify(paths):
    problems = []
    quotes = 0
    for name, path in paths.items():
        texts = extract_texts(path)
        if texts is None:
            print("WARNING: neither pypdf nor pdftotext is available; text checks skipped")
            return True
        for mode, text in texts.items():
            for s in SPEC[name]["present"]:
                if s not in text:
                    problems.append(f"{name}: {s!r} not in extracted text ({mode})")
            for s in SPEC[name]["absent"]:
                if s in text:
                    problems.append(f"{name}: {s!r} must not be printed ({mode})")
        expected = EXPECTED / f"{name}.json"
        if expected.exists():
            quotes += check_fields(json.loads(expected.read_text()), "", texts, problems)
        else:
            problems.append(f"{expected.relative_to(ROOT)} is missing")
    for p in problems:
        print("FAIL:", p)
    if not problems:
        print(f"OK: contract values present and {quotes} evidence quotes found in page 1 text ({', '.join(texts)})")
    return not problems


def main():
    SAMPLES.mkdir(exist_ok=True)
    WEB_COPY.mkdir(parents=True, exist_ok=True)
    paths = {}
    for name, doc in DOCS.items():
        paths[name] = write_pdf(name, doc)
        shutil.copyfile(paths[name], WEB_COPY / paths[name].name)
        print("wrote", paths[name].relative_to(ROOT))
    return 0 if verify(paths) else 1


if __name__ == "__main__":
    sys.exit(main())
