# Sample documents

Two synthetic shipments, each a one-page commercial invoice and packing list. **Sample A**
(`INV-2026-0042` / `PL-2026-0042`, porcelain, stoneware and bamboo goods, FOB Ningbo) has
deliberate problems for the cross-document checks to find. The invoice prints a total of
USD 7,302.00, but its lines add up to 7,320.00. The packing list shows 780 mugs against 800 on
the invoice, and calls them "Ceramic" where the invoice says "Stoneware" (a material word, so
the difference is a warning). It also prints 84 cartons, but its lines add up to 82 (the
936.00 kg gross weight is right). The plates are "white glazed" on the invoice only, which
is reported as an info-level difference. **Sample B** (`INV-2026-0057` / `PL-2026-0057`,
textiles, CIF Melbourne) is consistent and gives no error or warning findings, only an info
note that a CIF invoice prints no separate freight or insurance amounts. Its 0.385 unit price
tests a price quoted to a fraction of a cent (three decimal places). Neither invoice prints
freight, insurance or discount rows, so their `charges` lists are empty. All companies,
addresses and numbers are fictional.

`expected/` holds hand-checked extractions of each PDF in the domain schema (the `payload`
object, with every evidence quote copied exactly from the PDF text, quantities as decimal
strings such as `"1200"`, and `line_no` as the row's position). It also holds
`sample-a-candidates.json`: illustrative candidate codes for a licensed customs broker to
review, not answers. The offline fake model (`MODEL_PROVIDER=fake`) serves these files.
To regenerate the PDFs, run `make samples` or `python3 scripts/gen_samples.py` (needs
reportlab). The output is byte-for-byte reproducible, and the script copies the PDFs to
`web/public/samples/`. It then reads the text back and fails if any contract value or evidence
quote is missing. `go test ./samples/` checks the JSON files against the schema and runs the
real cross-document checks on them, so a change in the expected findings shows up as a
failing test.
