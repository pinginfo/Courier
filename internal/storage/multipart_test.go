package storage_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"testing"
)

func TestS3LargeObjectRoundTrip(t *testing.T) {
	s := newFakeS3(t, nil)
	ctx := context.Background()
	for _, size := range []int{1 << 20, 17 << 20, 33 << 20, 60 << 20} {
		data := make([]byte, size+12345) // not a multiple of the part size
		_, _ = rand.Read(data)
		key := "big/obj"
		if err := s.Put(ctx, key, bytes.NewReader(data), int64(len(data)), "application/octet-stream"); err != nil {
			t.Fatalf("size %d: put: %v", len(data), err)
		}
		info, err := s.Stat(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		rc, err := s.Get(ctx, key)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := io.ReadAll(rc)
		_ = rc.Close()
		t.Logf("size %d: stat=%d get=%d equal=%v", len(data), info.Size, len(got), bytes.Equal(got, data))
		if info.Size != int64(len(data)) || !bytes.Equal(got, data) {
			t.Errorf("size %d: object in store differs (stat %d, get %d bytes)", len(data), info.Size, len(got))
		}
	}
}
