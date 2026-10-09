package state

import (
	"context"
	"strings"
	"testing"
	"time"

	"courier/internal/storage"
)

func TestLoadMissingReturnsEmpty(t *testing.T) {
	m, err := Load(context.Background(), storage.NewMemory(), "alpine")
	if err != nil || len(m.Packages) != 0 || m.Orphans == nil {
		t.Fatalf("m=%+v err=%v", m, err)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	s := storage.NewMemory()
	m := New()
	m.Packages["a/b.apk"] = 42
	m.Metadata = []string{"a/APKINDEX.tar.gz"}
	m.Orphans["old.apk"] = Orphan{Since: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	if err := Save(context.Background(), s, "alpine", m); err != nil {
		t.Fatal(err)
	}
	got, err := Load(context.Background(), s, "alpine")
	if err != nil || got.Packages["a/b.apk"] != 42 || !got.Orphans["old.apk"].Since.Equal(m.Orphans["old.apk"].Since) {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestLoadRejectsCorruptAndFutureVersions(t *testing.T) {
	s := storage.NewMemory()
	_ = s.Put(context.Background(), Key("x"), strings.NewReader("{nope"), 5, "")
	if _, err := Load(context.Background(), s, "x"); err == nil {
		t.Error("corrupt manifest accepted")
	}
	_ = s.Put(context.Background(), Key("y"), strings.NewReader(`{"version":99}`), 14, "")
	if _, err := Load(context.Background(), s, "y"); err == nil {
		t.Error("unknown version accepted")
	}
}
