package verifier

import (
	"archive/zip"
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	pathpkg "path"
	"strings"
)

type Manifest struct {
	Version      int               `json:"version"`
	TenantID     string            `json:"tenant_id"`
	RecordCounts map[string]int    `json:"record_counts"`
	Files        map[string]string `json:"files"`
}
type reference struct{ dataset, field, id string }

func Verify(path string) error {
	z, e := zip.OpenReader(path)
	if e != nil {
		return e
	}
	defer z.Close()
	seen := map[string]*zip.File{}
	var m Manifest
	found := false
	var uncompressed uint64
	for _, f := range z.File {
		if _, ok := seen[f.Name]; ok {
			return fmt.Errorf("duplicate entry %q", f.Name)
		}
		if pathpkg.IsAbs(f.Name) || strings.Contains(f.Name, "\\") || pathpkg.Clean(f.Name) != f.Name || strings.HasPrefix(f.Name, "../") {
			return fmt.Errorf("unsafe archive path %q", f.Name)
		}
		seen[f.Name] = f
		uncompressed += f.UncompressedSize64
		if f.UncompressedSize64 > 128<<20 || uncompressed > 256<<20 {
			return fmt.Errorf("archive uncompressed size limit exceeded")
		}
		if f.Name == "manifest.json" {
			r, e := f.Open()
			if e != nil {
				return e
			}
			manifestBytes, re := io.ReadAll(io.LimitReader(r, (8<<20)+1))
			if re != nil {
				r.Close()
				return re
			}
			if len(manifestBytes) > 8<<20 {
				r.Close()
				return fmt.Errorf("manifest size limit exceeded")
			}
			e = json.Unmarshal(manifestBytes, &m)
			r.Close()
			if e != nil {
				return e
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("manifest missing")
	}
	if m.Version != 1 {
		return fmt.Errorf("unsupported manifest version")
	}
	if m.TenantID == "" {
		return fmt.Errorf("manifest tenant is missing")
	}
	if len(seen) != len(m.Files)+1 {
		return fmt.Errorf("archive has undeclared entries")
	}
	for dataset, n := range m.RecordCounts {
		if n < 0 {
			return fmt.Errorf("negative count for %s", dataset)
		}
	}
	checkedDatasets := map[string]bool{}
	ids := map[string]map[string]bool{}
	refs := []reference{}
	for name, want := range m.Files {
		if name == "manifest.json" || pathpkg.IsAbs(name) || pathpkg.Clean(name) != name || strings.Contains(name, "\\") {
			return fmt.Errorf("unsafe manifest path %q", name)
		}
		f := seen[name]
		if f == nil {
			return fmt.Errorf("missing %s", name)
		}
		r, e := f.Open()
		if e != nil {
			return e
		}
		h := sha256.New()
		count := -1
		if strings.HasPrefix(name, "data/") && strings.HasSuffix(name, ".jsonl") {
			count = 0
			dataset := strings.TrimSuffix(strings.TrimPrefix(name, "data/"), ".jsonl")
			scanner := bufio.NewScanner(io.TeeReader(r, h))
			scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
			for scanner.Scan() {
				if !json.Valid(scanner.Bytes()) {
					r.Close()
					return fmt.Errorf("invalid JSONL row in %s", name)
				}
				var record map[string]json.RawMessage
				if json.Unmarshal(scanner.Bytes(), &record) != nil {
					r.Close()
					return fmt.Errorf("invalid JSON row in %s", name)
				}
				if raw, ok := record["tenant_id"]; ok {
					var tenant string
					if json.Unmarshal(raw, &tenant) != nil || tenant != m.TenantID {
						r.Close()
						return fmt.Errorf("cross-tenant row in %s", name)
					}
				}
				if dataset == "tenants" || dataset == "users" || dataset == "projects" || dataset == "project_members" || dataset == "invoices" || dataset == "invoice_items" || dataset == "attachments" || dataset == "activity_events" {
					raw, ok := record["id"]
					var id string
					if !ok || json.Unmarshal(raw, &id) != nil || id == "" {
						r.Close()
						return fmt.Errorf("missing record ID in %s", dataset)
					}
					if ids[dataset] == nil {
						ids[dataset] = map[string]bool{}
					}
					if ids[dataset][id] {
						r.Close()
						return fmt.Errorf("duplicate record ID in %s", dataset)
					}
					ids[dataset][id] = true
					if dataset == "tenants" && id != m.TenantID {
						r.Close()
						return fmt.Errorf("foreign tenant record")
					}
				}
				add := func(target, field string) bool {
					raw := record[field]
					if len(raw) == 0 || string(raw) == "null" {
						return false
					}
					var id string
					if json.Unmarshal(raw, &id) == nil && id != "" {
						refs = append(refs, reference{target, field, id})
						return true
					}
					return false
				}
				switch dataset {
				case "projects":
					if !add("users", "owner_user_id") {
						r.Close()
						return fmt.Errorf("missing project owner")
					}
				case "project_members":
					if !add("projects", "project_id") || !add("users", "user_id") {
						r.Close()
						return fmt.Errorf("missing project membership reference")
					}
				case "invoices":
					if !add("projects", "project_id") {
						r.Close()
						return fmt.Errorf("missing invoice project")
					}
				case "invoice_items":
					if !add("invoices", "invoice_id") {
						r.Close()
						return fmt.Errorf("missing invoice item parent")
					}
				case "attachments":
					var owner string
					_ = json.Unmarshal(record["owner_type"], &owner)
					if owner == "project" {
						if !add("projects", "owner_id") {
							r.Close()
							return fmt.Errorf("missing attachment owner")
						}
					} else if owner == "invoice" {
						if !add("invoices", "owner_id") {
							r.Close()
							return fmt.Errorf("missing attachment owner")
						}
					} else {
						r.Close()
						return fmt.Errorf("invalid attachment owner type")
					}
				case "activity_events":
					if raw, ok := record["actor_user_id"]; ok && string(raw) != "null" && !add("users", "actor_user_id") {
						r.Close()
						return fmt.Errorf("invalid activity actor")
					}
				}
				count++
			}
			if scanner.Err() != nil {
				r.Close()
				return fmt.Errorf("read %s: %w", name, scanner.Err())
			}
		} else {
			if _, e = io.Copy(h, r); e != nil {
				r.Close()
				return e
			}
		}
		r.Close()
		if hex.EncodeToString(h.Sum(nil)) != want {
			return fmt.Errorf("checksum mismatch: %s", name)
		}
		if count >= 0 {
			dataset := strings.TrimSuffix(strings.TrimPrefix(name, "data/"), ".jsonl")
			n, ok := m.RecordCounts[dataset]
			if !ok || n != count {
				return fmt.Errorf("record count mismatch: %s", name)
			}
			checkedDatasets[dataset] = true
		}
	}
	if len(checkedDatasets) != len(m.RecordCounts) {
		return fmt.Errorf("manifest declares an absent dataset")
	}
	for _, ref := range refs {
		if ids[ref.dataset] == nil || !ids[ref.dataset][ref.id] {
			return fmt.Errorf("unresolved %s reference to %s", ref.field, ref.dataset)
		}
	}
	return nil
}
