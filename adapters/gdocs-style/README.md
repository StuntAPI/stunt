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
endpoint takes a list of structural requests (insertText, updateTextStyle,
etc.) that modify the content at specific indices. Getting the index math
right is a major pain point. This adapter lets you test the create →
batchUpdate → GET round-trip locally.

## Auth

- **Bearer:** `Authorization: Bearer <oauth2-token>`.

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| POST | `/v1/documents` | Create a document (`{title}`). |
| GET | `/v1/documents/{documentId}` | Get document with structural content. |
| POST | `/v1/documents/{documentId}/batchUpdate` | Batch structural updates (`{requests:[{insertText:{location:{index}, text}}, ...]}`). |
| GET | `/v1/documents/{documentId}/revisions` | List revisions. |

## Key shapes

- Document: `{documentId, title, body:{content:[{startIndex, endIndex, paragraph:{elements:[{startIndex, endIndex, textRun:{content}}], paragraphStyle:{}}}]}}`.
- batchUpdate request: `{requests:[{insertText:{location:{index:1}, text:"Hello"}}]}`.
- batchUpdate response: `{documentId, replies:[...]}`.

## Data model

Documents are **stateful**. batchUpdate insertText operations are applied
to the document content and are reflected in subsequent GET requests. A
default document is seeded on first access.
