package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func makeArchive(t *testing.T, mutate func(*manifest, map[string][]byte)) string {
	t.Helper()
	files := map[string][]byte{"data/users.jsonl": []byte("{\"id\":\"user-1\"}\n")}
	m := manifest{Version: 1, TenantID: "tenant-a", RecordCounts: map[string]int{"users": 1}, Files: map[string]string{}}
	mutate(&m, files)
	for n, b := range files {
		s := sha256.Sum256(b)
		m.Files[n] = hex.EncodeToString(s[:])
	}
	mb, _ := json.Marshal(m)
	p := filepath.Join(t.TempDir(), "export.zip")
	out, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	w := zip.NewWriter(out)
	for n, b := range files {
		f, e := w.Create(n)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = f.Write(b); e != nil {
			t.Fatal(e)
		}
	}
	f, e := w.Create("manifest.json")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.Write(mb); e != nil {
		t.Fatal(e)
	}
	if e = w.Close(); e != nil {
		t.Fatal(e)
	}
	if e = out.Close(); e != nil {
		t.Fatal(e)
	}
	return p
}

func TestVerifyValidArchive(t *testing.T) {
	p := makeArchive(t, func(*manifest, map[string][]byte) {})
	if err := verify(p); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyRejectsTamperedRows(t *testing.T) {
	p := makeArchive(t, func(_ *manifest, files map[string][]byte) { files["data/users.jsonl"] = []byte("not-json\n") })
	if err := verify(p); err == nil {
		t.Fatal("expected invalid JSONL to fail")
	}
}

func TestVerifyRejectsUnsafePath(t *testing.T) {
	p := makeArchive(t, func(_ *manifest, files map[string][]byte) { files["../outside.jsonl"] = []byte("{}\n") })
	if err := verify(p); err == nil {
		t.Fatal("expected unsafe path to fail")
	}
}

func TestVerifyRejectsForeignTenantRow(t *testing.T) {
	p := makeArchive(t, func(_ *manifest, files map[string][]byte) {
		files["data/users.jsonl"] = []byte("{\"id\":\"user-2\",\"tenant_id\":\"tenant-b\"}\n")
	})
	if err := verify(p); err == nil {
		t.Fatal("expected cross-tenant row to fail")
	}
}

func TestVerifyRejectsUnresolvedRelationship(t *testing.T) {
	p := makeArchive(t, func(m *manifest, files map[string][]byte) {
		delete(files, "data/users.jsonl")
		files["data/projects.jsonl"] = []byte("{\"id\":\"project-1\",\"owner_user_id\":\"missing-user\"}\n")
		m.RecordCounts = map[string]int{"projects": 1}
	})
	if err := verify(p); err == nil {
		t.Fatal("expected unresolved project owner reference to fail")
	}
}

func TestVerifyDetectsArchiveTampering(t *testing.T) {
	p := makeArchive(t, func(*manifest, map[string][]byte) {})
	in, e := zip.OpenReader(p)
	if e != nil {
		t.Fatal(e)
	}
	target := filepath.Join(t.TempDir(), "changed.zip")
	out, e := os.Create(target)
	if e != nil {
		t.Fatal(e)
	}
	zw := zip.NewWriter(out)
	for _, entry := range in.File {
		r, e := entry.Open()
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(r)
		r.Close()
		if e != nil {
			t.Fatal(e)
		}
		if entry.Name == "data/users.jsonl" {
			data = []byte("{\"id\":\"user-2\"}\n")
		}
		w, e := zw.Create(entry.Name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write(data); e != nil {
			t.Fatal(e)
		}
	}
	in.Close()
	if e = zw.Close(); e != nil {
		t.Fatal(e)
	}
	if e = out.Close(); e != nil {
		t.Fatal(e)
	}
	if e = verify(target); e == nil {
		t.Fatal("expected modified data to fail the manifest digest")
	}
}
