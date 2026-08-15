# gdocs-style

A stunt adapter simulating the **Google Docs API** with the structural
content-editing model, for local testing.

## Simulated API

- **Name:** Google Docs API
- **Version:** `v1`

## Why this adapter?

Google Docs uses a notoriously complex structural content model: document
text is represented as a list of ranged elements (paragraphs containing
textRuns with startIndex/endIndex), not as plain text. The batchUpdate
endpoint takes a list of structural requests that modify the content at
specific indices. Getting the index math right is a major pain point. This
adapter lets you test the create → batchUpdate → GET round-trip locally.

## Auth

- **Bearer:** `Authorization: Bearer <oauth2-token>`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v1/documents` | Create a document (`{title}`). |
| GET | `/v1/documents/{documentId}` | Get document with structural content (paragraphs, named styles, bullets, page breaks, inline objects). |
| POST | `/v1/documents/{documentId}/batchUpdate` | Batch structural updates (see the request vocabulary below). Serialized per-document (`concurrency_key: documentId`). |
| GET | `/v1/documents/{documentId}/revisions` | List revisions. |

## Document shape

```json
{
  "documentId": "…",
  "title": "Untitled document",
  "body": {
    "content": [
      {
        "startIndex": 1,
        "endIndex": 14,
        "paragraph": {
          "elements": [
            {"startIndex": 1, "endIndex": 14,
             "textRun": {"content": "Hello, World!\n", "textStyle": {}}}
          ],
          "paragraphStyle": {"namedStyle": "NORMAL_TEXT"}
        }
      }
    ]
  },
  "inlineObjects": {"kix.…": {"objectId": "kix.…", "inlineObjectProperties": {…}}},
  "lists": {"list.…": {"listProperties": {"nestingLevels": [{"glyph": "•"}]}}},
  "documentStyle": {…},
  "suggestionsViewMode": "PREVIEW_WITHOUT_SUGGESTIONS"
}
```

`inlineObjects` and `lists` appear only when the document contains any.
Every paragraph implicitly owns one trailing `\n` (its endIndex covers it),
and the trailing newline is rendered inside the paragraph's last textRun,
like the real API. Non-text elements appear as `{"pageBreak": {}}` or
`{"inlineObjectElement": {"inlineObjectId": "…"}}` entries occupying one
index each.

## Indices are UTF-16 code units

Like the real service, every index is 1-based and counted in UTF-16 code
units. An astral character (e.g. an emoji) occupies **two** consecutive
indices (its surrogate pair), a BMP character one. Consequently:

- inserting `"😀"` at index 1 shifts everything after it by **2**;
- an index that falls strictly between the high and low half of a surrogate
  pair is invalid and returns `400 INVALID_ARGUMENT`;
- `deleteContentRange` boundaries must not split a surrogate pair either.

## batchUpdate request vocabulary

| Request | Shape |
|---------|-------|
| `insertText` | `{location: {index}, text}` or `{endOfSegmentLocation: {}, text}`. `\n` in the text splits paragraphs. |
| `deleteContentRange` | `{range: {startIndex, endIndex}}` (endIndex exclusive). Deleting a paragraph's newline merges it with the next paragraph. The document's final newline cannot be deleted. |
| `updateParagraphStyle` | `{range, paragraphStyle: {namedStyle, alignment, …}, fields: "namedStyle"}`. Valid named styles: `NORMAL_TEXT`, `HEADING_1`–`HEADING_6`, `TITLE`, `SUBTITLE`. |
| `updateTextStyle` | `{range, textStyle: {bold, italic, fontSize, …}, fields: "bold,italic"}`. The fields mask selects what to apply; omitted payload values clear the field. |
| `createParagraphBullets` | `{range, bulletPreset}`. All paragraphs in the call join one new list (`lists` map in GET). |
| `deleteParagraphBullets` | `{range}`. |
| `insertPageBreak` | `{location: {index}}`. |
| `insertInlineImage` | `{location: {index}, uri, objectSize}` (uri reference) **or** inline image bytes — `{location, imageData: {data: <base64>, mimeType}}` (also accepted flat as `data`/`mimeType`). Returns `{objectId}`. |

Request-level rules:

- requests are applied **in order** within one batchUpdate call;
- each request object must set **exactly one** request type;
- an unknown request type returns `400 INVALID_ARGUMENT` (it is not a
  silent no-op);
- out-of-range indices/ranges return `400 INVALID_ARGUMENT`.

### Image bytes

`insertInlineImage` accepts image data three ways:

1. **JSON base64** — `imageData: {data: <base64>, mimeType: "image/png"}`
   on the request. Standard and URL-safe base64 are both accepted.
2. **multipart/form-data body** — send the whole batchUpdate as multipart
   with a `metadata` part (the JSON `{requests: [...]}`) plus one file part
   per image; parts are consumed by `insertInlineImage` requests in order.
   Bytes are stored in the service's blob store under the generated
   objectId.
3. **uri only** — reference an externally uploaded image; the document
   records the uri as `imageProperties.contentUri` and no bytes are stored.

## Data model

Documents are **stateful**. batchUpdate operations are applied to the
document's paragraph model (persisted under `model` on the record) and are
reflected in subsequent GET requests; the ranged `body.content` is derived
from the model on read. A default document is seeded on first access.

Simplifications (documented deviations from the real API):

- `replies` maps 1:1 to requests (requests with no output reply `{}`; the
  real API can return a shorter array);
- inserted text adopts the style of the run it lands in;
- pressing Enter (inserting `\n`) continues the paragraph's bullet and named
  style into the new paragraph;
- tables, headers/footers, suggestions and `writeControl` are not modeled.
