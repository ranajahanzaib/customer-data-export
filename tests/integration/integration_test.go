package integration

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ranajahanzaib/customer-data-export/internal/app"
	"github.com/ranajahanzaib/customer-data-export/internal/storage"
)

func requireIntegration(t *testing.T) {
	t.Helper()
	if os.Getenv("RUN_INTEGRATION") != "1" {
		t.Skip("set RUN_INTEGRATION=1 against the Compose PostgreSQL and MinIO services")
	}
}
func TestPostgresTenantSeedAndCompositeIsolation(t *testing.T) {
	requireIntegration(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, e := pgxpool.New(ctx, os.Getenv("DATABASE_URL"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	var a, b int
	if e = db.QueryRow(ctx, "SELECT count(*) FROM projects WHERE tenant_id=$1", "10000000-0000-4000-8000-000000000001").Scan(&a); e != nil {
		t.Fatal(e)
	}
	if e = db.QueryRow(ctx, "SELECT count(*) FROM projects WHERE tenant_id=$1", "10000000-0000-4000-8000-000000000002").Scan(&b); e != nil {
		t.Fatal(e)
	}
	if a != 1 || b != 1 {
		t.Fatalf("seed project counts A=%d B=%d", a, b)
	}
	_, e = db.Exec(ctx, `INSERT INTO project_members(tenant_id,project_id,user_id,membership_role) VALUES($1,$2,$3,'member')`, "10000000-0000-4000-8000-000000000001", "30000000-0000-4000-8000-000000000002", "20000000-0000-4000-8000-000000000001")
	if e == nil {
		t.Fatal("cross-tenant membership unexpectedly satisfied composite foreign keys")
	}
}
func TestMinIOSeedAttachmentChecksum(t *testing.T) {
	requireIntegration(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s := storage.New(env("S3_ENDPOINT", "http://localhost:9000"), env("S3_REGION", "us-east-1"), env("S3_ACCESS_KEY", "localminio"), env("S3_SECRET_KEY", "localminio_secret"))
	r, e := s.Get(ctx, "attachments", "10000000-0000-4000-8000-000000000001/acme-brief.txt")
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	got, e := io.ReadAll(r)
	if e != nil {
		t.Fatal(e)
	}
	sum := sha256.Sum256(got)
	if hex.EncodeToString(sum[:]) != "0c4d45ac7f06d08bcbb9edff330d919fd558a58e74592377d52d9182cb78f418" {
		t.Fatalf("seed attachment checksum mismatch: %x", sum)
	}
}

func TestAPIAdministratorAuthorizationAndTenantIsolation(t *testing.T) {
	requireIntegration(t)
	t.Setenv("ARTIFACT_DIR", t.TempDir())
	t.Setenv("APP_ENV", "development")
	t.Setenv("JWT_HMAC_SECRET", "integration-test-signing-secret-at-least-32-bytes")
	t.Setenv("JWT_ISSUER", "customer-data-export-local")
	t.Setenv("JWT_AUDIENCE", "customer-data-export")
	a, e := app.New()
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	h := a.Handler()
	member := apiToken(t, "20000000-0000-4000-8000-000000000004", "10000000-0000-4000-8000-000000000001")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/exports", bytes.NewBufferString(`{"format":"zip","datasets":["all"]}`))
	req.Header.Set("Authorization", "Bearer "+member)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member request status %d", rec.Code)
	}
	adminA := apiToken(t, "20000000-0000-4000-8000-000000000001", "10000000-0000-4000-8000-000000000001")
	req = httptest.NewRequest(http.MethodPost, "/api/v1/exports", bytes.NewBufferString(`{"format":"zip","datasets":["all"]}`))
	req.Header.Set("Authorization", "Bearer "+adminA)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("admin request status %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID string `json:"id"`
	}
	if e = json.Unmarshal(rec.Body.Bytes(), &created); e != nil {
		t.Fatal(e)
	}
	adminB := apiToken(t, "20000000-0000-4000-8000-000000000002", "10000000-0000-4000-8000-000000000002")
	req = httptest.NewRequest(http.MethodGet, "/api/v1/exports/"+created.ID, nil)
	req.Header.Set("Authorization", "Bearer "+adminB)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status returned %d", rec.Code)
	}
}

func apiToken(t *testing.T, user, tenant string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	claims, _ := json.Marshal(map[string]any{"sub": user, "tenant_id": tenant, "iss": "customer-data-export-local", "aud": "customer-data-export", "exp": time.Now().Add(time.Hour).Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	mac := hmac.New(sha256.New, []byte("integration-test-signing-secret-at-least-32-bytes"))
	_, _ = mac.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
