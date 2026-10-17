package audit_test

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/ShifuRain/convertflow/internal/audit"
)

func TestEventHasIPAndNoBody(t *testing.T) {
	var buf bytes.Buffer
	l := audit.New(&buf)
	r := httptest.NewRequest("POST", "/api/v1/jobs", bytes.NewBufferString("SECRET-CONTENT"))
	r.RemoteAddr = "192.0.2.1:5555"
	l.Event(r, "job_created", "abc", "preset", "audio-wav")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["event"] != "job_created" || m["job_id"] != "abc" || m["remote_ip"] != "192.0.2.1" || m["audit"] != true {
		t.Fatalf("bad record: %v", m)
	}
	if bytes.Contains(buf.Bytes(), []byte("SECRET-CONTENT")) {
		t.Fatal("request body leaked into audit log")
	}
}

func TestSystemEvent(t *testing.T) {
	var buf bytes.Buffer
	audit.New(&buf).System("job_started", "id1", "worker", "w1")
	var m map[string]any
	_ = json.Unmarshal(buf.Bytes(), &m)
	if m["event"] != "job_started" || m["worker"] != "w1" {
		t.Fatalf("bad record: %v", m)
	}
}
