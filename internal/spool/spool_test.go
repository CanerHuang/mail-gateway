package spool_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"maild/internal/spool"
)

func TestName_RoundTrip(t *testing.T) {
	cases := []struct {
		name spool.Name
		want string
	}{
		{spool.Name{Due: time.Unix(1761552000, 0), MailID: "01JBXK3Q9V8M2T4N6P0R5S7W9Y"}, "1761552000_01JBXK3Q9V8M2T4N6P0R5S7W9Y.mail"},
		{spool.Name{Due: time.Unix(1761552000, 0), MailID: "01JBXK3Q9V8M2T4N6P0R5S7W9Y", Failures: 2}, "1761552000_01JBXK3Q9V8M2T4N6P0R5S7W9Y_2.mail"},
	}
	for _, tc := range cases {
		if got := tc.name.String(); got != tc.want {
			t.Errorf("String() = %q, want %q", got, tc.want)
		}
		parsed, ok := spool.ParseName(tc.want)
		if !ok || !parsed.Due.Equal(tc.name.Due) || parsed.MailID != tc.name.MailID || parsed.Failures != tc.name.Failures {
			t.Errorf("ParseName(%q) = %+v, %v", tc.want, parsed, ok)
		}
	}
	for _, bad := range []string{"x.mail", "1_ID_0.mail", "1_ID_x.mail", "1_ID.eml", "-1_ID.mail", "1__2.mail", "1_ID_2_3.mail"} {
		if _, ok := spool.ParseName(bad); ok {
			t.Errorf("ParseName(%q) should fail", bad)
		}
	}
}

func TestSpool_WriteAndOpen(t *testing.T) {
	dir := t.TempDir()
	// 啟動時 tmp/ 裡的殘留檔要被清掉。
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tmp", "leftover"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := spool.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(entries) != 0 {
		t.Errorf("tmp/ should be cleaned, has %d entries", len(entries))
	}

	name := spool.Name{Due: time.Unix(1761552000, 0), MailID: "01JBXK3Q9V8M2T4N6P0R5S7W9Y"}
	meta := &spool.Meta{
		MailID:  name.MailID,
		Client:  "order-service",
		Sender:  "service",
		From:    "service@example.com",
		To:      []string{"a@example.com"},
		Bcc:     []string{"c@example.com"},
		Subject: "訂單\n<通知>",
		Labels:  map[string]string{"order_id": "A1234"},
	}
	mimeBody := strings.Repeat("line of mail content\r\n", 5000) // 比讀取緩衝區大，確認串流讀取不會遺失內容
	err = s.Write(name, meta, func(w io.Writer) error {
		_, err := io.WriteString(w, mimeBody)
		return err
	})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, "queue", name.String()))
	if err != nil {
		t.Fatalf("queue file missing: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("queue file permission = %o, want 600", perm)
	}

	names, invalid, err := s.List()
	if err != nil || len(names) != 1 || len(invalid) != 0 {
		t.Fatalf("List = %v, %v, %v", names, invalid, err)
	}

	entry, err := s.OpenEntry(names[0])
	if err != nil {
		t.Fatalf("OpenEntry: %v", err)
	}
	defer entry.Close()
	if entry.Meta.Subject != meta.Subject || entry.Meta.Labels["order_id"] != "A1234" {
		t.Errorf("meta = %+v", entry.Meta)
	}
	if strings.Join(entry.Meta.To, ",") != "a@example.com" || strings.Join(entry.Meta.Bcc, ",") != "c@example.com" {
		t.Errorf("recipients = %v / %v", entry.Meta.To, entry.Meta.Bcc)
	}
	body, err := io.ReadAll(entry.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != mimeBody {
		t.Errorf("body mismatch: got %d bytes, want %d", len(body), len(mimeBody))
	}
	if entry.Size != int64(len(mimeBody)) {
		t.Errorf("Size = %d, want %d", entry.Size, len(mimeBody))
	}
}

func TestSpool_RescheduleAndRemove(t *testing.T) {
	s, err := spool.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := spool.Name{Due: time.Unix(100, 0), MailID: "ID1"}
	if err := s.Write(name, &spool.Meta{MailID: "ID1"}, func(io.Writer) error { return nil }); err != nil {
		t.Fatal(err)
	}
	next := spool.Name{Due: time.Unix(200, 0), MailID: "ID1", Failures: 1}
	if err := s.Reschedule(name, next); err != nil {
		t.Fatalf("Reschedule: %v", err)
	}
	if _, err := s.OpenEntry(name); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("old name should be gone, got %v", err)
	}
	if err := s.Remove(next); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if names, _, _ := s.List(); len(names) != 0 {
		t.Errorf("queue should be empty, got %v", names)
	}
}

func TestSpool_Corrupted(t *testing.T) {
	dir := t.TempDir()
	s, err := spool.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"1_NOLINE.mail":   strings.Repeat("x", 70*1024),
		"1_BADJSON.mail":  "not json\nbody",
		"1_MISMATCH.mail": `{"mail_id":"OTHER"}` + "\nbody",
	}
	for filename, content := range cases {
		if err := os.WriteFile(filepath.Join(dir, "queue", filename), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		name, _ := spool.ParseName(filename)
		if _, err := s.OpenEntry(name); !errors.Is(err, spool.ErrCorrupted) {
			t.Errorf("%s: want ErrCorrupted, got %v", filename, err)
		}
	}
}
