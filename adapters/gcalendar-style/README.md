# gcalendar-style

A stunt adapter simulating the **Google Calendar API** with events, recurring
events, and quickAdd, for local testing.

## Simulated API

- **Name:** Google Calendar API
- **Version:** `v3`

## Why this adapter?

Google Calendar's event model includes recurring events (RRULE expansion),
iCal UIDs, access roles, and natural-language quickAdd. This adapter lets you
test your Calendar integration locally without the OAuth2 consent verification
process.

## Endpoints

### Calendars (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/calendar/v3/calendars/primary` | Get primary calendar (`{id, summary, timeZone, accessRole}`). |
| GET | `/calendar/v3/users/me/calendarList` | List calendars (`{items:[{id, summary, primary, ...}]}`). |

### Events (Bearer required)

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/calendar/v3/calendars/{calendarId}/events` | List events (params: `timeMin`, `timeMax`, `maxResults`, `singleEvents`, `orderBy`). |
| POST | `/calendar/v3/calendars/{calendarId}/events` | Create event (`{summary, start, end, attendees, recurrence}`). |
| GET | `/calendar/v3/calendars/{calendarId}/events/{eventId}` | Get event. |
| PATCH | `/calendar/v3/calendars/{calendarId}/events/{eventId}` | Patch event. |
| DELETE | `/calendar/v3/calendars/{calendarId}/events/{eventId}` | Delete event (204). |
| GET | `/calendar/v3/calendars/{calendarId}/events/{eventId}/instances` | Expand recurring event instances. |
| POST | `/calendar/v3/calendars/{calendarId}/events/quickAdd?text=` | Natural-language quick add. |
| POST | `/calendar/v3/calendars/{calendarId}/events/import` | Import event (preserves iCalUID). |

## Key shapes

- Event: `{id, summary, start:{dateTime, timeZone}, end:{dateTime, timeZone}, attendees:[{email, responseStatus}], status, htmlLink, creator:{email}, iCalUID, recurrence:["RRULE:FREQ=DAILY;COUNT=3"]}`.
- Event list: `{kind:"calendar#events", items:[...], nextPageToken?}`.
- Recurring instance: same shape + `recurringEventId`.

## Data model fidelity

- **iCal UIDs**: events have both `id` (hex) and `iCalUID` (`<hex>@google.com`).
- **Recurring events**: `recurrence:["RRULE:FREQ=DAILY;COUNT=3"]` is stored and
  expanded into individual instances when listed with `singleEvents=true` or via
  the `/instances` endpoint. DAILY and WEEKLY frequencies are supported.
- **timeMin/timeMax filtering**: ISO8601 string comparison on `start.dateTime`.
- A default calendar (`mock-user@gmail.com`) with a seeded event is created on
  first access.
