package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ranajahanzaib/customer-data-export/internal/storage"
	"github.com/ranajahanzaib/customer-data-export/internal/verifier"
)

type App struct {
	DB                             *pgxpool.Pool
	key                            []byte
	issuer, audience, artifactDir  string
	s3                             *storage.S3
	attachmentBucket, exportBucket string
}

//go:embed dashboard.html
var dashboard embed.FS

type principal struct{ UserID, TenantID, Role string }
type claims struct {
	Sub    string `json:"sub"`
	Tenant string `json:"tenant_id"`
	Iss    string `json:"iss"`
	Aud    string `json:"aud"`
	Exp    int64  `json:"exp"`
}
type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

func New() (*App, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://export:export_dev_only@localhost:5432/customer_export?sslmode=disable"
	}
	c, e := pgxpool.New(context.Background(), dsn)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if e = c.Ping(ctx); e != nil {
		c.Close()
		return nil, e
	}
	key := []byte(os.Getenv("JWT_HMAC_SECRET"))
	if len(key) < 32 {
		if os.Getenv("APP_ENV") == "production" {
			c.Close()
			return nil, errors.New("JWT_HMAC_SECRET must contain at least 32 bytes in production")
		}
		key = []byte("local-development-signing-secret-not-for-production")
	}
	if os.Getenv("APP_ENV") == "production" && (string(key) == "local-development-signing-secret-not-for-production" || os.Getenv("JWT_ISSUER") == "" || os.Getenv("JWT_AUDIENCE") == "") {
		c.Close()
		return nil, errors.New("production requires a non-development JWT secret, issuer, and audience")
	}
	if os.Getenv("APP_ENV") == "production" && (os.Getenv("DATABASE_URL") == "" || strings.Contains(dsn, "sslmode=disable") || os.Getenv("S3_ENDPOINT") == "" || !strings.HasPrefix(os.Getenv("S3_ENDPOINT"), "https://") || os.Getenv("S3_ACCESS_KEY") == "" || os.Getenv("S3_SECRET_KEY") == "" || os.Getenv("ATTACHMENT_BUCKET") == "" || os.Getenv("EXPORT_BUCKET") == "") {
		c.Close()
		return nil, errors.New("production requires TLS PostgreSQL and explicitly configured TLS S3 credentials and buckets")
	}
	dir := os.Getenv("ARTIFACT_DIR")
	if dir == "" {
		dir = "./artifacts"
	}
	if e = os.MkdirAll(dir, 0700); e != nil {
		c.Close()
		return nil, e
	}
	region := val("S3_REGION", "us-east-1")
	return &App{DB: c, key: key, issuer: val("JWT_ISSUER", "customer-data-export-local"), audience: val("JWT_AUDIENCE", "customer-data-export"), artifactDir: dir, s3: storage.New(val("S3_ENDPOINT", "http://localhost:9000"), region, val("S3_ACCESS_KEY", "localminio"), val("S3_SECRET_KEY", "localminio_secret")), attachmentBucket: val("ATTACHMENT_BUCKET", "attachments"), exportBucket: val("EXPORT_BUCKET", "exports")}, nil
}
func (a *App) Close() { a.DB.Close() }
func val(s, d string) string {
	if v := os.Getenv(s); v != "" {
		return v
	}
	return d
}
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/dashboard", http.StatusTemporaryRedirect)
	})
	m.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeFileFS(w, r, dashboard, "dashboard.html")
	})
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) })
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, c := context.WithTimeout(r.Context(), 2*time.Second)
		defer c()
		if e := a.DB.Ping(ctx); e != nil {
			http.Error(w, "not ready", 503)
			return
		}
		w.Write([]byte("ready\n"))
	})
	m.HandleFunc("POST /api/v1/exports", a.auth(a.request))
	m.HandleFunc("GET /api/v1/exports", a.auth(a.list))
	m.HandleFunc("GET /api/v1/exports/{id}", a.auth(a.status))
	m.HandleFunc("POST /api/v1/exports/{id}/cancel", a.auth(a.cancel))
	m.HandleFunc("GET /api/v1/exports/{id}/manifest", a.auth(a.manifest))
	m.HandleFunc("GET /api/v1/exports/{id}/download", a.auth(a.download))
	return m
}
func (a *App) auth(next func(http.ResponseWriter, *http.Request, principal)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, e := a.authenticate(r)
		if e != nil {
			http.Error(w, "unauthorized", 401)
			return
		}
		next(w, r, p)
	}
}
func (a *App) authenticate(r *http.Request) (principal, error) {
	var p principal
	parts := strings.Split(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), ".")
	if len(parts) != 3 {
		return p, errors.New("token")
	}
	headerBytes, he := base64.RawURLEncoding.DecodeString(parts[0])
	var header jwtHeader
	if he != nil || json.Unmarshal(headerBytes, &header) != nil || header.Alg != "HS256" || header.Typ != "JWT" {
		return p, errors.New("unsupported JWT header")
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	sig, e := base64.RawURLEncoding.DecodeString(parts[2])
	if e != nil || !hmac.Equal(sig, mac.Sum(nil)) {
		return p, errors.New("signature")
	}
	raw, e := base64.RawURLEncoding.DecodeString(parts[1])
	if e != nil {
		return p, e
	}
	var c claims
	if json.Unmarshal(raw, &c) != nil || c.Sub == "" || c.Tenant == "" || c.Iss != a.issuer || c.Aud != a.audience || c.Exp < time.Now().Unix() {
		return p, errors.New("claims")
	}
	e = a.DB.QueryRow(r.Context(), "SELECT role FROM users WHERE id=$1 AND tenant_id=$2 AND status='active'", c.Sub, c.Tenant).Scan(&p.Role)
	if e != nil {
		return p, e
	}
	p.UserID = c.Sub
	p.TenantID = c.Tenant
	return p, nil
}
func (a *App) request(w http.ResponseWriter, r *http.Request, p principal) {
	if !a.requireAdmin(w, r, p, "export.request") {
		return
	}
	var body struct {
		Format   string   `json:"format"`
		Datasets []string `json:"datasets"`
	}
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(&body) != nil || (body.Format != "" && body.Format != "zip") || (len(body.Datasets) > 0 && !(len(body.Datasets) == 1 && body.Datasets[0] == "all")) {
		http.Error(w, "invalid request", 400)
		return
	}
	var trailing any
	if d.Decode(&trailing) != io.EOF {
		http.Error(w, "invalid request", 400)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if len(key) > 128 {
		http.Error(w, "invalid idempotency key", 400)
		return
	}
	hash := sha256.Sum256([]byte("zip:all"))
	var id, status string
	err := a.DB.QueryRow(r.Context(), `INSERT INTO export_jobs(tenant_id,requested_by,idempotency_key,request_hash) VALUES($1,$2,NULLIF($3,''),$4) ON CONFLICT(tenant_id,requested_by,idempotency_key) WHERE idempotency_key IS NOT NULL DO UPDATE SET idempotency_key=EXCLUDED.idempotency_key WHERE export_jobs.request_hash=EXCLUDED.request_hash RETURNING id,status`, p.TenantID, p.UserID, key, hex.EncodeToString(hash[:])).Scan(&id, &status)
	if err != nil {
		http.Error(w, "idempotency key conflicts with a different request", 409)
		return
	}
	a.audit(r.Context(), p, "export.request", "allowed")
	writeJSON(w, 202, map[string]string{"id": id, "status": status, "status_url": "/api/v1/exports/" + id})
}
func (a *App) list(w http.ResponseWriter, r *http.Request, p principal) {
	if !a.requireAdmin(w, r, p, "export.list") {
		return
	}
	limit, offset := 50, 0
	if value := r.URL.Query().Get("limit"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 1 || n > 100 {
			http.Error(w, "invalid limit", 400)
			return
		}
		limit = n
	}
	if value := r.URL.Query().Get("offset"); value != "" {
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 || n > 10000 {
			http.Error(w, "invalid offset", 400)
			return
		}
		offset = n
	}
	rows, e := a.DB.Query(r.Context(), "SELECT id,status,progress_phase,record_count,attachment_count,requested_at FROM export_jobs WHERE tenant_id=$1 ORDER BY requested_at DESC,id DESC LIMIT $2 OFFSET $3", p.TenantID, limit, offset)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var id, st, phase string
		var records, attachments int64
		var at time.Time
		if rows.Scan(&id, &st, &phase, &records, &attachments, &at) == nil {
			out = append(out, map[string]any{"id": id, "status": st, "phase": phase, "record_count": records, "attachment_count": attachments, "requested_at": at})
		}
	}
	if len(out) == limit {
		w.Header().Set("X-Next-Offset", strconv.Itoa(offset+limit))
	}
	writeJSON(w, 200, out)
}
func (a *App) status(w http.ResponseWriter, r *http.Request, p principal) {
	if !a.requireAdmin(w, r, p, "export.status") {
		return
	}
	var id, status, phase, verify, code, msg string
	var records, att, bytes int64
	var tries int
	var requested time.Time
	var started, completed, expires *time.Time
	err := a.DB.QueryRow(r.Context(), `SELECT id,status,progress_phase,verification_status,COALESCE(failure_code,''),COALESCE(failure_message,''),record_count,attachment_count,bytes_written,attempt_count,requested_at,started_at,completed_at,expires_at FROM export_jobs WHERE id=$1 AND tenant_id=$2`, r.PathValue("id"), p.TenantID).Scan(&id, &status, &phase, &verify, &code, &msg, &records, &att, &bytes, &tries, &requested, &started, &completed, &expires)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "status": status, "phase": phase, "verification": verify, "failure_code": code, "error": msg, "record_count": records, "attachment_count": att, "bytes_written": bytes, "attempt_count": tries, "requested_at": requested, "started_at": started, "completed_at": completed, "expires_at": expires})
}
func (a *App) cancel(w http.ResponseWriter, r *http.Request, p principal) {
	if !a.requireAdmin(w, r, p, "export.cancel") {
		return
	}
	tag, e := a.DB.Exec(r.Context(), "UPDATE export_jobs SET status='canceled',canceled_at=now(),lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND tenant_id=$2 AND status IN ('queued','running')", r.PathValue("id"), p.TenantID)
	if e != nil {
		http.Error(w, "database error", 500)
		return
	}
	if tag.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(204)
}
func (a *App) manifest(w http.ResponseWriter, r *http.Request, p principal) {
	if !a.requireAdmin(w, r, p, "export.manifest") {
		return
	}
	var key, archiveSHA, manifestSHA string
	if a.DB.QueryRow(r.Context(), "SELECT archive_object_key,archive_sha256,manifest_sha256 FROM export_jobs WHERE id=$1 AND tenant_id=$2 AND status='available' AND verification_status='passed' AND expires_at>now()", r.PathValue("id"), p.TenantID).Scan(&key, &archiveSHA, &manifestSHA) != nil {
		http.NotFound(w, r)
		return
	}
	b, e := os.ReadFile(filepath.Join(a.artifactDir, filepath.Base(key)))
	if e != nil {
		remote, re := a.s3.Get(r.Context(), a.exportBucket, key)
		if re != nil {
			http.NotFound(w, r)
			return
		}
		defer remote.Close()
		b, e = io.ReadAll(io.LimitReader(remote, 32<<20))
		if e != nil {
			http.Error(w, "artifact unavailable", 502)
			return
		}
	}
	if digest(b) != strings.TrimSpace(archiveSHA) {
		http.Error(w, "artifact integrity check failed", 500)
		return
	}
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		http.Error(w, "artifact invalid", 500)
		return
	}
	for _, f := range z.File {
		if f.Name == "manifest.json" {
			rc, e := f.Open()
			if e != nil {
				break
			}
			manifest, e := io.ReadAll(io.LimitReader(rc, 8<<20))
			rc.Close()
			if e != nil {
				http.Error(w, "manifest unavailable", 500)
				return
			}
			if digest(manifest) != strings.TrimSpace(manifestSHA) {
				http.Error(w, "manifest integrity check failed", 500)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write(manifest)
			return
		}
	}
	http.Error(w, "manifest missing", 500)
}
func (a *App) download(w http.ResponseWriter, r *http.Request, p principal) {
	if !a.requireAdmin(w, r, p, "export.download") {
		return
	}
	var key string
	if a.DB.QueryRow(r.Context(), "SELECT archive_object_key FROM export_jobs WHERE id=$1 AND tenant_id=$2 AND status='available' AND verification_status='passed' AND expires_at>now()", r.PathValue("id"), p.TenantID).Scan(&key) != nil {
		http.NotFound(w, r)
		return
	}
	url, e := a.s3.PresignGetAt(val("S3_PUBLIC_ENDPOINT", a.s3.Endpoint), a.exportBucket, key, 60*time.Second)
	if e != nil {
		http.Error(w, "download unavailable", 502)
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "application/json") {
		a.audit(r.Context(), p, "export.download", "allowed")
		writeJSON(w, http.StatusOK, map[string]any{"url": url, "expires_in": 60})
		return
	}
	http.Redirect(w, r, url, http.StatusFound)
	a.audit(r.Context(), p, "export.download", "allowed")
}
func (a *App) requireAdmin(w http.ResponseWriter, r *http.Request, p principal, action string) bool {
	if p.Role == "admin" {
		return true
	}
	a.audit(r.Context(), p, action, "denied")
	http.Error(w, "forbidden", http.StatusForbidden)
	return false
}
func (a *App) audit(ctx context.Context, p principal, action, outcome string) {
	_, _ = a.DB.Exec(ctx, "INSERT INTO audit_events(tenant_id,actor_user_id,action,outcome) VALUES($1,$2,$3,$4)", p.TenantID, p.UserID, action, outcome)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

type ds struct{ name, sql, inventorySQL string }

var datasets = []ds{
	{"tenants", `SELECT id::text AS id,name,slug,status,created_at,updated_at FROM tenants WHERE id=$1 ORDER BY id`, `SELECT count(*) FROM tenants WHERE id=$1`},
	{"users", `SELECT id::text AS id,tenant_id::text AS tenant_id,email,name,role,status,created_at,updated_at FROM users WHERE tenant_id=$1 ORDER BY id`, `SELECT count(*) FROM users WHERE tenant_id=$1`},
	{"projects", `SELECT id::text AS id,tenant_id::text AS tenant_id,name,description,status,owner_user_id::text AS owner_user_id,created_at,updated_at FROM projects WHERE tenant_id=$1 ORDER BY id`, `SELECT count(*) FROM projects WHERE tenant_id=$1`},
	{"project_members", `SELECT id::text AS id,tenant_id::text AS tenant_id,project_id::text AS project_id,user_id::text AS user_id,membership_role,created_at FROM project_members WHERE tenant_id=$1 ORDER BY id`, `SELECT count(*) FROM project_members WHERE tenant_id=$1`},
	{"invoices", `SELECT id::text AS id,tenant_id::text AS tenant_id,invoice_number,project_id::text AS project_id,amount::text AS amount,currency,status,issued_at,due_at,created_at FROM invoices WHERE tenant_id=$1 ORDER BY id`, `SELECT count(*) FROM invoices WHERE tenant_id=$1`},
	{"invoice_items", `SELECT id::text AS id,tenant_id::text AS tenant_id,invoice_id::text AS invoice_id,description,quantity::text AS quantity,unit_price::text AS unit_price,amount::text AS amount FROM invoice_items WHERE tenant_id=$1 ORDER BY id`, `SELECT count(*) FROM invoice_items WHERE tenant_id=$1`},
	{"attachments", `SELECT id::text AS id,tenant_id::text AS tenant_id,owner_type,owner_id::text AS owner_id,original_filename,content_type,object_key,object_version_id,content_size,sha256,created_at FROM attachments WHERE tenant_id=$1 ORDER BY id`, `SELECT count(*) FROM attachments WHERE tenant_id=$1`},
	{"activity_events", `SELECT id::text AS id,tenant_id::text AS tenant_id,actor_user_id::text AS actor_user_id,action,entity_type,entity_id::text AS entity_id,metadata,occurred_at FROM activity_events WHERE tenant_id=$1 ORDER BY id`, `SELECT count(*) FROM activity_events WHERE tenant_id=$1`},
}

func (a *App) Work(ctx context.Context) error {
	worker := val("WORKER_ID", "worker-local")
	for ctx.Err() == nil {
		a.cleanupExpired(ctx)
		var id, tenant string
		var nonce [16]byte
		if _, e := rand.Read(nonce[:]); e != nil {
			return e
		}
		lease := worker + ":" + hex.EncodeToString(nonce[:])
		_, _ = a.DB.Exec(ctx, `UPDATE export_jobs SET status='failed',failure_code='attempts_exhausted',failure_message='Worker attempts exhausted',lease_owner=NULL,lease_expires_at=NULL WHERE status='running' AND lease_expires_at<now() AND attempt_count>=max_attempts`)
		e := a.DB.QueryRow(ctx, `UPDATE export_jobs SET status='running',lease_owner=$1,lease_expires_at=now()+interval '5 minutes',attempt_count=attempt_count+1,started_at=COALESCE(started_at,now()),progress_phase='snapshot' WHERE id=(SELECT id FROM export_jobs WHERE (status='queued' OR (status='running' AND lease_expires_at<now())) AND attempt_count<max_attempts ORDER BY requested_at FOR UPDATE SKIP LOCKED LIMIT 1) RETURNING id,tenant_id`, lease).Scan(&id, &tenant)
		if errors.Is(e, pgx.ErrNoRows) {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
				continue
			}
		}
		if e != nil {
			return e
		}
		if e = a.process(ctx, id, tenant, lease); e != nil {
			slog.Error("export failed", "export_id", id, "error", e)
			_, _ = a.DB.Exec(context.Background(), `UPDATE export_jobs SET status=CASE WHEN attempt_count<max_attempts THEN 'queued' ELSE 'failed' END, failure_code='export_failed',failure_message='Export could not be completed',lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND lease_owner=$2`, id, lease)
		}
	}
	return nil
}
func (a *App) process(ctx context.Context, id, tenant, worker string) error {
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.Background())
	defer stopHeartbeat()
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-heartbeatCtx.Done():
				return
			case <-ticker.C:
				_, _ = a.DB.Exec(heartbeatCtx, `UPDATE export_jobs SET lease_expires_at=now()+interval '5 minutes' WHERE id=$1 AND status='running' AND lease_owner=$2`, id, worker)
			}
		}
	}()
	tx, e := a.DB.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	files := map[string][]byte{}
	counts := map[string]int{}
	total := 0
	var stagedBytes int64
	for _, d := range datasets {
		var jobStatus string
		if e = a.DB.QueryRow(ctx, "SELECT status FROM export_jobs WHERE id=$1", id).Scan(&jobStatus); e != nil {
			return e
		}
		if jobStatus == "canceled" {
			return errors.New("export canceled")
		}
		var inventory int
		if e = tx.QueryRow(ctx, d.inventorySQL, tenant).Scan(&inventory); e != nil {
			return e
		}
		if d.name == "attachments" {
			if e = a.collectAttachments(ctx, tx, tenant, files, counts); e != nil {
				return e
			}
			if counts["attachments"] != inventory {
				return fmt.Errorf("attachment inventory mismatch")
			}
			total += counts["attachments"]
			if e = a.updateProgress(ctx, id, worker, d.name, total, counts["attachments"], files); e != nil {
				return e
			}
			continue
		}
		rows, e := tx.Query(ctx, d.sql, tenant)
		if e != nil {
			return e
		}
		cols := rows.FieldDescriptions()
		var b bytes.Buffer
		for rows.Next() {
			vals, e := rows.Values()
			if e != nil {
				rows.Close()
				return e
			}
			obj := map[string]any{}
			for i, c := range cols {
				value := vals[i]
				if raw, ok := value.([]byte); ok {
					var decoded any
					if json.Unmarshal(raw, &decoded) == nil {
						value = decoded
					}
				}
				obj[string(c.Name)] = value
			}
			line, e := json.Marshal(obj)
			if e != nil {
				rows.Close()
				return e
			}
			if len(line) > 8<<20 {
				rows.Close()
				return errors.New("record exceeds 8 MiB limit")
			}
			b.Write(line)
			b.WriteByte('\n')
			if b.Len() > 128<<20 {
				rows.Close()
				return errors.New("dataset exceeds 128 MiB limit")
			}
			counts[d.name]++
			total++
		}
		if e = rows.Err(); e != nil {
			rows.Close()
			return e
		}
		rows.Close()
		if counts[d.name] != inventory {
			return fmt.Errorf("%s inventory mismatch", d.name)
		}
		files["data/"+d.name+".jsonl"] = b.Bytes()
		stagedBytes += int64(b.Len())
		if stagedBytes > 128<<20 {
			return errors.New("export exceeds configured staging limit")
		}
		if b.Len() > 128<<20 {
			return errors.New("export exceeds memory safety limit")
		}
		if e = a.updateProgress(ctx, id, worker, d.name, total, counts["attachments"], files); e != nil {
			return e
		}
	}
	var stagedTotal int64
	for _, payload := range files {
		stagedTotal += int64(len(payload))
	}
	if stagedTotal > 128<<20 {
		return errors.New("export exceeds configured staging limit")
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	digests := map[string]string{}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		b := files[n]
		s := sha256.Sum256(b)
		digests[n] = hex.EncodeToString(s[:])
	}
	m := map[string]any{"version": 1, "tenant_id": tenant, "created_at": time.Now().UTC(), "record_counts": counts, "files": digests}
	mb, _ := json.MarshalIndent(m, "", "  ")
	files["manifest.json"] = mb
	archiveNames := append(names, "manifest.json")
	sort.Strings(archiveNames)
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range archiveNames {
		b := files[n]
		f, e := zw.Create(n)
		if e != nil {
			return e
		}
		if _, e = f.Write(b); e != nil {
			return e
		}
	}
	if e = zw.Close(); e != nil {
		return e
	}
	if buf.Len() > 128<<20 {
		return errors.New("export exceeds configured archive size limit")
	}
	sum := sha256.Sum256(buf.Bytes())
	name := id + ".zip"
	path := filepath.Join(a.artifactDir, name)
	if e = os.WriteFile(path, buf.Bytes(), 0600); e != nil {
		return e
	}
	if e = verifyArchive(path); e != nil {
		_ = os.Remove(path)
		return e
	}
	if e = a.s3.Put(ctx, a.exportBucket, name, bytes.NewReader(buf.Bytes()), int64(buf.Len()), "application/zip"); e != nil {
		_ = os.Remove(path)
		return e
	}
	_ = os.Remove(path)
	tag, e := a.DB.Exec(ctx, `UPDATE export_jobs SET status='available',progress_phase='complete',record_count=$3,archive_object_key=$4,archive_sha256=$5,manifest_sha256=$6,verification_status='passed',completed_at=now(),expires_at=now()+interval '7 days',bytes_written=$7,attachment_count=$8,lease_owner=NULL,lease_expires_at=NULL WHERE id=$1 AND lease_owner=$2 AND status='running'`, id, worker, total, name, hex.EncodeToString(sum[:]), digest(mb), buf.Len(), counts["attachments"])
	if e != nil {
		_ = a.s3.Delete(context.Background(), a.exportBucket, name)
		return e
	}
	if tag.RowsAffected() != 1 {
		_ = a.s3.Delete(context.Background(), a.exportBucket, name)
		return errors.New("worker lease lost before publication")
	}
	return nil
}

func (a *App) updateProgress(ctx context.Context, id, lease, phase string, records, attachments int, files map[string][]byte) error {
	var size int64
	for _, b := range files {
		size += int64(len(b))
	}
	tag, e := a.DB.Exec(ctx, `UPDATE export_jobs SET progress_phase=$3,record_count=$4,attachment_count=$5,bytes_written=$6 WHERE id=$1 AND lease_owner=$2 AND status='running'`, id, lease, phase, records, attachments, size)
	if e != nil {
		return e
	}
	if tag.RowsAffected() != 1 {
		return errors.New("worker lease lost or export canceled")
	}
	return nil
}

func (a *App) cleanupExpired(ctx context.Context) {
	rows, e := a.DB.Query(ctx, `SELECT id,archive_object_key FROM export_jobs WHERE status='available' AND expires_at<now() ORDER BY expires_at LIMIT 25`)
	if e != nil {
		return
	}
	type expired struct{ id, key string }
	items := []expired{}
	for rows.Next() {
		var x expired
		if rows.Scan(&x.id, &x.key) == nil {
			items = append(items, x)
		}
	}
	rows.Close()
	for _, x := range items {
		if a.s3.Delete(ctx, a.exportBucket, x.key) != nil {
			continue
		}
		_, _ = a.DB.Exec(ctx, `UPDATE export_jobs SET status='expired',archive_object_key=NULL WHERE id=$1 AND status='available' AND expires_at<now()`, x.id)
	}
}

func (a *App) collectAttachments(ctx context.Context, tx pgx.Tx, tenant string, files map[string][]byte, counts map[string]int) error {
	type attachment struct {
		id, ownerType, ownerID, filename, contentType, key, version, want string
		size                                                              int64
		ownerValid                                                        bool
	}
	rows, e := tx.Query(ctx, `SELECT a.id,a.owner_type,a.owner_id,a.original_filename,a.content_type,a.object_key,COALESCE(a.object_version_id,''),a.content_size,a.sha256,CASE WHEN a.owner_type='project' THEN EXISTS(SELECT 1 FROM projects p WHERE p.tenant_id=a.tenant_id AND p.id=a.owner_id) WHEN a.owner_type='invoice' THEN EXISTS(SELECT 1 FROM invoices i WHERE i.tenant_id=a.tenant_id AND i.id=a.owner_id) ELSE FALSE END FROM attachments a WHERE a.tenant_id=$1 ORDER BY a.id`, tenant)
	if e != nil {
		return e
	}
	defer rows.Close()
	var jsonl bytes.Buffer
	var binaryBytes int64
	for rows.Next() {
		var v attachment
		if e = rows.Scan(&v.id, &v.ownerType, &v.ownerID, &v.filename, &v.contentType, &v.key, &v.version, &v.size, &v.want, &v.ownerValid); e != nil {
			return e
		}
		if v.size < 0 || v.size > 64<<20 {
			return fmt.Errorf("attachment size limit exceeded")
		}
		binaryBytes += v.size
		if binaryBytes > 128<<20 {
			return fmt.Errorf("attachment staging limit exceeded")
		}
		if !v.ownerValid {
			return fmt.Errorf("attachment owner is outside tenant snapshot")
		}
		if strings.Contains(v.key, "..") || strings.HasPrefix(v.key, "/") || !strings.HasPrefix(v.key, tenant+"/") {
			return fmt.Errorf("unsafe attachment object key")
		}
		body, e := a.s3.GetVersion(ctx, a.attachmentBucket, v.key, v.version)
		if e != nil {
			return e
		}
		data, e := io.ReadAll(io.LimitReader(body, (64<<20)+1))
		closeErr := body.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if int64(len(data)) != v.size {
			return fmt.Errorf("attachment size mismatch")
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != strings.TrimSpace(v.want) {
			return fmt.Errorf("attachment checksum mismatch")
		}
		safe := filepath.Base(v.filename)
		safe = strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
				return r
			}
			return '_'
		}, safe)
		archiveName := "attachments/" + v.id + "/" + safe
		files[archiveName] = data
		meta := map[string]any{"id": v.id, "tenant_id": tenant, "owner_type": v.ownerType, "owner_id": v.ownerID, "original_filename": v.filename, "content_type": v.contentType, "content_size": v.size, "sha256": hex.EncodeToString(sum[:]), "file": archiveName}
		line, _ := json.Marshal(meta)
		jsonl.Write(line)
		jsonl.WriteByte('\n')
		counts["attachments"]++
	}
	if e = rows.Err(); e != nil {
		return e
	}
	files["data/attachments.jsonl"] = jsonl.Bytes()
	return nil
}
func digest(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func verifyArchive(path string) error {
	return verifier.Verify(path)
}
