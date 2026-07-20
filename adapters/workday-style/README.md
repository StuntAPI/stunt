# Workday-style adapter

A stunt adapter for simulating a **Workday REST API** (v40.0) locally.
All data is synthetic — no real API data is included.

> **Unofficial / not affiliated.** This adapter is not affiliated with, endorsed
> by, or sponsored by Workday. "Workday" and related marks are trademarks of
> their respective owners. See [DISCLAIMER](DISCLAIMER) for full terms. This
> adapter is for **local development and testing only**.

## What it simulates

A faithful behavioral mock of Workday's REST API surface (the REST equivalents
of Workday's SOAP services), designed to unblock HRMS/financial integrations
during local development:

- **Workers:** `GET /wbs/v40.0/staffing/workers`, `GET .../workers/{id}`.
- **Compensation:** `GET /wbs/v40.0/compensation/workers/{id}/compensation`.
- **Payroll:** `GET /wbs/v40.0/payroll/pay_components`.
- **Positions:** `GET /wbs/v40.0/human_resources/positions`.
- **Financials:** `GET /wbs/v40.0/financials/accounts`.
- **RaaS (Reports as a Service):** `GET /ccx/v1/{tenant}/RaaS/Custom_Report`
  — Workday's Custom Reports-as-a-Service, returning `{"Report_Entry": [...]}`.
- **Custom endpoint:** `POST /ccx/v1/{tenant}/staffing/Create_Worker` — model a
  create.

Workers are **stateful**: a worker created via `Create_Worker` appears in the
workers list.

## SOAP note

Workday's **primary interface is SOAP** (huge WSDLs with hundreds of operations).
This mock covers the **REST surface** only. The SOAP envelope shape is:

```xml
<soapenv:Envelope xmlns:soapenv="http://schemas.xmlsoap.org/soap/envelope/"
                  xmlns:wd="urn:com.workday/bsvc">
  <soapenv:Header>
    <wd:Workday_Common_Header>
      <wd:Include_Reference_Descriptors_In_Response>true</wd:Include_Reference_Descriptors_In_Response>
    </wd:Workday_Common_Header>
  </soapenv:Header>
  <soapenv:Body>
    <wd:Get_Workers_Request>
      <wd:Request_References>
        <wd:Worker_Reference>
          <wd:ID wd:type="Employee_ID">1</wd:ID>
        </wd:Worker_Reference>
      </wd:Request_References>
    </wd:Get_Workers_Request>
  </soapenv:Body>
</soapenv:Envelope>
```

This adapter does NOT parse SOAP; it provides the REST-equivalent endpoints that
most modern integrations use.

## Authentication

This adapter supports **OAuth Bearer tokens** and **Basic auth**:

```
Authorization: Bearer <access_token>
Authorization: Basic <base64(username:password)>
```

Workday tenant names appear in the URL path (e.g. `/ccx/v1/{tenant}/...`). This
mock validates the presence of an Authorization header. Requests without
authentication return **401**.

## List response shape

```json
{
  "data": [
    {
      "id": "1",
      "descriptor": "John Smith (jsmith)",
      "workerID": {"id": "1"},
      "primaryWorkEmail": "jsmith@example.net",
      "primaryEmploymentReference": {"id": "1", "descriptor": "Regular Full-Time"}
    }
  ],
  "total": 3,
  "more": false
}
```

## Error shape

```json
{
  "errors": [
    {
      "errorCode": "RESOURCE_NOT_FOUND",
      "errorDescription": "Worker '999' not found."
    }
  ]
}
```

## Endpoints

| Method | Route | Description |
|--------|-------|-------------|
| GET | `/wbs/v40.0/staffing/workers` | List workers |
| GET | `/wbs/v40.0/staffing/workers/{id}` | Get worker |
| GET | `/wbs/v40.0/compensation/workers/{id}/compensation` | Worker compensation |
| GET | `/wbs/v40.0/payroll/pay_components` | Pay components |
| GET | `/wbs/v40.0/human_resources/positions` | Positions |
| GET | `/wbs/v40.0/financials/accounts` | Financial accounts |
| GET | `/ccx/v1/{tenant}/RaaS/Custom_Report` | RaaS custom report |
| POST | `/ccx/v1/{tenant}/staffing/Create_Worker` | Create worker (RaaS) |
