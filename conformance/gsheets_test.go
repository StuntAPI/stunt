package conformance

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/api/sheets/v4"
)

// TestSheetsConformance drives Google's own generated client
// (google.golang.org/api/sheets/v4) against the gsheets-style adapter:
// option.WithEndpoint points the SDK at the booted engine (whose routes
// are rooted at /v4/spreadsheets, matching the SDK's path templates)
// while its serialization, A1-range path escaping, query-param plumbing
// and googleapi error decoding all stay stock.
func TestSheetsConformance(t *testing.T) {
	ctx := context.Background()
	base := Boot(t, "gsheets-style")

	svc, err := sheets.NewService(ctx,
		option.WithEndpoint(base),
		option.WithTokenSource(oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "conformance-token"})),
	)
	if err != nil {
		t.Fatalf("sheets.NewService: %v", err)
	}

	// ===== Spreadsheets.Create + Get round-trip =====
	created, err := svc.Spreadsheets.Create(&sheets.Spreadsheet{
		Properties: &sheets.SpreadsheetProperties{Title: "SDK Conformance"},
		Sheets:     []*sheets.Sheet{{Properties: &sheets.SheetProperties{Title: "Sheet1"}}},
	}).Do()
	if err != nil {
		t.Fatalf("spreadsheets.create: %v", err)
	}
	if created.SpreadsheetId == "" {
		t.Fatalf("spreadsheets.create: empty spreadsheetId")
	}
	if created.Properties == nil || created.Properties.Title != "SDK Conformance" {
		t.Errorf("spreadsheets.create: properties = %+v", created.Properties)
	}
	if len(created.Sheets) != 1 || created.Sheets[0].Properties == nil || created.Sheets[0].Properties.Title != "Sheet1" {
		t.Errorf("spreadsheets.create: sheets = %+v", created.Sheets)
	}
	if gp := created.Sheets[0].Properties.GridProperties; gp == nil || gp.RowCount != 1000 || gp.ColumnCount != 26 {
		t.Errorf("spreadsheets.create: gridProperties = %+v", created.Sheets[0].Properties.GridProperties)
	}
	if !strings.Contains(created.SpreadsheetUrl, created.SpreadsheetId) {
		t.Errorf("spreadsheets.create: spreadsheetUrl = %q (want id %q)", created.SpreadsheetUrl, created.SpreadsheetId)
	}
	meta, err := svc.Spreadsheets.Get(created.SpreadsheetId).Do()
	if err != nil {
		t.Fatalf("spreadsheets.get: %v", err)
	}
	if meta.SpreadsheetId != created.SpreadsheetId || meta.Properties.Title != "SDK Conformance" {
		t.Errorf("spreadsheets.get: id=%q title=%q", meta.SpreadsheetId, meta.Properties.Title)
	}
	Record(t, "google-api-go-client", "gsheets-style", "Spreadsheets.Create + Get round-trip")

	ssID := created.SpreadsheetId

	// ===== Values.Update writes cells that Values.Get reads back =====
	upd, err := svc.Spreadsheets.Values.Update(ssID, "Sheet1!A1:B3", &sheets.ValueRange{
		Values: [][]interface{}{{"Name", "Score"}, {"Alice", "95"}, {"Bob", "87"}},
	}).ValueInputOption("RAW").Do()
	if err != nil {
		t.Fatalf("values.update: %v", err)
	}
	if upd.UpdatedRange != "Sheet1!A1:B3" || upd.UpdatedRows != 3 || upd.UpdatedColumns != 2 || upd.UpdatedCells != 6 {
		t.Errorf("values.update: %+v", upd)
	}
	vr, err := svc.Spreadsheets.Values.Get(ssID, "Sheet1!A1:B3").Do()
	if err != nil {
		t.Fatalf("values.get: %v", err)
	}
	if vr.Range != "Sheet1!A1:B3" || vr.MajorDimension != "ROWS" {
		t.Errorf("values.get: range=%q majorDimension=%q", vr.Range, vr.MajorDimension)
	}
	wantRow(t, vr, 0, "Name", "Score")
	wantRow(t, vr, 1, "Alice", "95")
	wantRow(t, vr, 2, "Bob", "87")
	Record(t, "google-api-go-client", "gsheets-style", "Values.Update + Values.Get round-trip an A1 range")

	// ===== Values.Get MajorDimension=COLUMNS transposes the grid =====
	cols, err := svc.Spreadsheets.Values.Get(ssID, "Sheet1!A1:B3").MajorDimension("COLUMNS").Do()
	if err != nil {
		t.Fatalf("values.get COLUMNS: %v", err)
	}
	if cols.MajorDimension != "COLUMNS" {
		t.Errorf("values.get COLUMNS: majorDimension = %q", cols.MajorDimension)
	}
	wantRow(t, cols, 0, "Name", "Alice", "Bob")
	wantRow(t, cols, 1, "Score", "95", "87")
	Record(t, "google-api-go-client", "gsheets-style", "Values.Get MajorDimension=COLUMNS transposes the grid")

	// ===== Values.Append writes after the last populated row =====
	app, err := svc.Spreadsheets.Values.Append(ssID, "Sheet1!A1:B10", &sheets.ValueRange{
		Values: [][]interface{}{{"Carol", "76"}},
	}).ValueInputOption("RAW").Do()
	if err != nil {
		t.Fatalf("values.append: %v", err)
	}
	if app.TableRange != "Sheet1!A1:B3" {
		t.Errorf("values.append: tableRange = %q, want Sheet1!A1:B3", app.TableRange)
	}
	if app.Updates == nil || app.Updates.UpdatedRange != "Sheet1!A4:B4" || app.Updates.UpdatedRows != 1 || app.Updates.UpdatedCells != 2 {
		t.Errorf("values.append: updates = %+v", app.Updates)
	}
	appVR, err := svc.Spreadsheets.Values.Get(ssID, "Sheet1!A1:B10").Do()
	if err != nil {
		t.Fatalf("values.get after append: %v", err)
	}
	if len(appVR.Values) != 4 {
		t.Fatalf("values.get after append: %d rows, want 4 (%v)", len(appVR.Values), appVR.Values)
	}
	wantRow(t, appVR, 3, "Carol", "76")
	Record(t, "google-api-go-client", "gsheets-style", "Values.Append writes after the last populated row")

	// ===== Values.BatchUpdate writes multiple ranges in one call =====
	bu, err := svc.Spreadsheets.Values.BatchUpdate(ssID, &sheets.BatchUpdateValuesRequest{
		ValueInputOption: "RAW",
		Data: []*sheets.ValueRange{
			{Range: "Sheet1!D1:D2", Values: [][]interface{}{{"X"}, {"Y"}}},
			{Range: "Sheet1!E1:E2", Values: [][]interface{}{{"Z"}, {"W"}}},
		},
	}).Do()
	if err != nil {
		t.Fatalf("values.batchUpdate: %v", err)
	}
	if bu.TotalUpdatedRows != 4 || bu.TotalUpdatedColumns != 2 || bu.TotalUpdatedCells != 4 {
		t.Errorf("values.batchUpdate: totals = %+v", bu)
	}
	if len(bu.Responses) != 2 || bu.Responses[0].UpdatedRange != "Sheet1!D1:D2" || bu.Responses[1].UpdatedRange != "Sheet1!E1:E2" {
		t.Errorf("values.batchUpdate: responses = %+v", bu.Responses)
	}
	deVR, err := svc.Spreadsheets.Values.Get(ssID, "Sheet1!D1:E2").Do()
	if err != nil {
		t.Fatalf("values.get D1:E2: %v", err)
	}
	wantRow(t, deVR, 0, "X", "Z")
	wantRow(t, deVR, 1, "Y", "W")
	Record(t, "google-api-go-client", "gsheets-style", "Values.BatchUpdate writes multiple ranges in one call")

	// ===== Values.BatchGet reads a range over GET query params =====
	bg, err := svc.Spreadsheets.Values.BatchGet(ssID).Ranges("Sheet1!A1:B2").Do()
	if err != nil {
		t.Fatalf("values.batchGet: %v", err)
	}
	if len(bg.ValueRanges) != 1 {
		t.Fatalf("values.batchGet: %d valueRanges, want 1 (%+v)", len(bg.ValueRanges), bg.ValueRanges)
	}
	if bg.ValueRanges[0].Range != "Sheet1!A1:B2" {
		t.Errorf("values.batchGet: range = %q, want Sheet1!A1:B2", bg.ValueRanges[0].Range)
	}
	wantRow(t, bg.ValueRanges[0], 0, "Name", "Score")
	wantRow(t, bg.ValueRanges[0], 1, "Alice", "95")
	Record(t, "google-api-go-client", "gsheets-style", "Values.BatchGet reads a range over GET query params")

	// ===== Spreadsheets.BatchUpdate addSheet adds a tab Values can write to =====
	sbu, err := svc.Spreadsheets.BatchUpdate(ssID, &sheets.BatchUpdateSpreadsheetRequest{
		Requests: []*sheets.Request{{
			AddSheet: &sheets.AddSheetRequest{Properties: &sheets.SheetProperties{Title: "Stats"}},
		}},
	}).Do()
	if err != nil {
		t.Fatalf("spreadsheets.batchUpdate: %v", err)
	}
	if len(sbu.Replies) != 1 || sbu.Replies[0].AddSheet == nil {
		t.Fatalf("spreadsheets.batchUpdate: replies = %+v", sbu.Replies)
	}
	added := sbu.Replies[0].AddSheet.Properties
	if added == nil || added.Title != "Stats" || added.SheetId == 0 {
		t.Errorf("spreadsheets.batchUpdate: addSheet properties = %+v", added)
	}
	afterMeta, err := svc.Spreadsheets.Get(ssID).Do()
	if err != nil {
		t.Fatalf("spreadsheets.get after addSheet: %v", err)
	}
	titles := map[string]bool{}
	for _, s := range afterMeta.Sheets {
		titles[s.Properties.Title] = true
	}
	if !titles["Sheet1"] || !titles["Stats"] {
		t.Errorf("spreadsheets.get: sheet titles = %v, want Sheet1+Stats", titles)
	}
	if _, err := svc.Spreadsheets.Values.Update(ssID, "Stats!A1:B1", &sheets.ValueRange{
		Values: [][]interface{}{{"Metric", "Value"}},
	}).ValueInputOption("RAW").Do(); err != nil {
		t.Fatalf("values.update on Stats: %v", err)
	}
	statsVR, err := svc.Spreadsheets.Values.Get(ssID, "Stats!A1:B1").Do()
	if err != nil {
		t.Fatalf("values.get on Stats: %v", err)
	}
	wantRow(t, statsVR, 0, "Metric", "Value")
	Record(t, "google-api-go-client", "gsheets-style", "Spreadsheets.BatchUpdate addSheet adds a tab Values can write to")

	// ===== Unknown spreadsheet IDs decode as googleapi 404s =====
	_, err = svc.Spreadsheets.Get("1sdk-conformance-missing-0000000").Do()
	if gErr, ok := err.(*googleapi.Error); !ok || gErr.Code != 404 {
		t.Errorf("spreadsheets.get unknown id: want googleapi 404, got %v", err)
	}
	Record(t, "google-api-go-client", "gsheets-style", "Get on an unknown spreadsheet -> googleapi 404")
}

// wantRow asserts one row of a ValueRange as strings (cells come back as
// the adapter-written strings, so %v is the lossless comparison).
func wantRow(t *testing.T, vr *sheets.ValueRange, i int, want ...string) {
	t.Helper()
	if i >= len(vr.Values) {
		t.Fatalf("row %d: only %d rows in %v", i, len(vr.Values), vr.Values)
	}
	got := make([]string, 0, len(vr.Values[i]))
	for _, v := range vr.Values[i] {
		got = append(got, fmt.Sprintf("%v", v))
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("row %d: got %v, want %v", i, got, want)
	}
}
