# gsheets-style

A stunt adapter simulating the **Google Sheets API** with the A1-notation grid
model, for local testing.

## Simulated API

- **Name:** Google Sheets API
- **Version:** `v4`

## Why this adapter?

Google Sheets' data model is notoriously unintuitive: cells are addressed via
A1 notation (`Sheet1!A1:C3`), values are returned as 2D arrays, and the
write/read round-trip has surprising semantics. This adapter lets you test
your Sheets integration locally without the multi-week OAuth2 consent
verification process.

## Endpoints

### Spreadsheets (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v4/spreadsheets` | Create a spreadsheet (`{properties:{title}, sheets}`). |
| GET | `/v4/spreadsheets/{spreadsheetId}` | Get spreadsheet metadata (sheets, properties). |
| POST | `/v4/spreadsheets/{spreadsheetId}/sheets` | Add a sheet (tab). |
| POST | `/v4/spreadsheets/{spreadsheetId}:batchUpdate` | Batch operations (`requests:[{addSheet, deleteSheet, updateCells, ...}]`). |

### Values (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/v4/spreadsheets/{spreadsheetId}/values/{range}` | Read cells (`range` = A1 notation). |
| PUT | `/v4/spreadsheets/{spreadsheetId}/values/{range}` | Write cells (`{values:[[...]], valueInputOption}`). |
| POST | `/v4/spreadsheets/{spreadsheetId}/values/{range}:append` | Append rows after last data. |
| POST | `/v4/spreadsheets/{spreadsheetId}/values/{range}:clear` | Clear cells. |
| POST | `/v4/spreadsheets/{spreadsheetId}/values:batchGet` | Read multiple ranges. |
| POST | `/v4/spreadsheets/{spreadsheetId}/values:batchUpdate` | Write multiple ranges. |

## Key shapes

- Spreadsheet: `{spreadsheetId, properties:{title, locale, timeZone}, sheets:[{properties:{sheetId, title, index, gridProperties:{rowCount, columnCount}}}]}`.
- Values response: `{range, majorDimension:"ROWS", values:[["a","b"],["c","d"]]}`.
- Update response: `{spreadsheetId, updatedRange, updatedRows, updatedColumns, updatedCells}`.
- Append response: `{spreadsheetId, tableRange, updates:{updatedRange, ...}}`.

## Data model fidelity

- **A1 notation**: full parse of `Sheet1!A1:C3`, `Sheet1!A1`, `A1:C3`, `A1` and
  column-only ranges like `A:C`. Column letters → 0-indexed (`A=0`, `Z=25`, `AA=26`).
- **Stateful grid**: cells written via PUT are readable by a subsequent GET.
- **Trailing-empty trimming**: values arrays have trailing empty rows/columns
  trimmed, matching the real API.
- A default spreadsheet (`Test Spreadsheet`) is seeded with sample data on
  first access.
