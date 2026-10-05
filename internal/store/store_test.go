package store

import (
	"path/filepath"
	"testing"
)

func TestOpenCreatesUsableStore(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
}

func TestSavePlacementKeepsFirstRecord(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "store.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()

	node := "node-a"
	if _, _, created, err := st.SavePlacement("ns", "n", "q", &node, []byte(`{"v":1}`), []byte(`{"status":"placed"}`)); err != nil || !created {
		t.Fatalf("first save: created=%v err=%v", created, err)
	}

	request, record, created, err := st.SavePlacement("ns", "n", "q", nil, []byte(`{"v":2}`), []byte(`{"status":"rejected"}`))
	if err != nil {
		t.Fatalf("second save: %v", err)
	}
	if created {
		t.Fatalf("second save created a new record")
	}
	if string(request) != `{"v":1}` || string(record) != `{"status":"placed"}` {
		t.Fatalf("stored = %s / %s, want the first submission", request, record)
	}

	got, found, err := st.GetPlacement("ns", "n")
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if string(got) != `{"status":"placed"}` {
		t.Fatalf("get = %s", got)
	}
	if _, found, err := st.GetPlacement("ns", "missing"); err != nil || found {
		t.Fatalf("get missing: found=%v err=%v", found, err)
	}
}
