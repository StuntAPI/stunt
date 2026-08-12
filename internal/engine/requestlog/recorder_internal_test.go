package requestlog

import (
	"bytes"
	"net/http/httptest"
	"testing"
)

// TestCapturingWriterCapsBuffer proves the response-capture buffer is capped at
// ~maxBody while every byte is written through to the client. Without the cap
// (the #5 bug) a multi-MB media response was duplicated in memory for the whole
// request lifetime even though only 64 KB is persisted.
func TestCapturingWriterCapsBuffer(t *testing.T) {
	big := bytes.Repeat([]byte("x"), 8*(maxBody+1)) // well over the capture cap
	client := httptest.NewRecorder()
	cw := &capturingWriter{ResponseWriter: client}

	n, err := cw.Write(big)
	if err != nil || n != len(big) {
		t.Fatalf("Write returned (%d, %v), want (%d, nil)", n, err, len(big))
	}
	if cw.buf.Len() > maxBody+1 {
		t.Errorf("capture buffer = %d bytes, want <= maxBody+1 (%d) — response duplicated in memory", cw.buf.Len(), maxBody+1)
	}
	if client.Body.Len() != len(big) {
		t.Errorf("client body = %d bytes, want full %d (write-through broken)", client.Body.Len(), len(big))
	}

	// A second Write after the cap is reached must not grow the buffer further.
	cw.Write(big)
	if cw.buf.Len() > maxBody+1 {
		t.Errorf("capture buffer grew past cap on 2nd write: %d bytes", cw.buf.Len())
	}
}
