package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/ranajahanzaib/customer-data-export/internal/verifier"
)

type manifest = verifier.Manifest

var exportIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func main() {
	args := os.Args[1:]
	if len(args) == 3 && args[0] == "token" {
		t, e := token(args[1], args[2])
		if e != nil {
			fatal(e)
		}
		fmt.Println(t)
		return
	}
	if len(args) == 2 && args[0] == "verify" {
		if e := verify(args[1]); e != nil {
			fatal(e)
		}
		fmt.Println("verification: PASSED")
		return
	}
	if e := apiCommand(args); e != nil {
		fatal(e)
	}
}
func fatal(e error) { fmt.Fprintln(os.Stderr, e); os.Exit(1) }
func token(user, tenant string) (string, error) {
	key := os.Getenv("JWT_HMAC_SECRET")
	if len(key) < 32 {
		return "", fmt.Errorf("JWT_HMAC_SECRET must be at least 32 bytes")
	}
	issuer, audience := os.Getenv("JWT_ISSUER"), os.Getenv("JWT_AUDIENCE")
	if issuer == "" {
		issuer = "customer-data-export-local"
	}
	if audience == "" {
		audience = "customer-data-export"
	}
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{"sub": user, "tenant_id": tenant, "iss": issuer, "aud": audience, "exp": time.Now().Add(time.Hour).Unix()})
	unsigned := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	h := hmac.New(sha256.New, []byte(key))
	_, _ = h.Write([]byte(unsigned))
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(h.Sum(nil)), nil
}
func verify(path string) error { return verifier.Verify(path) }
func apiCommand(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: exportctl request|list|status ID|download ID|verify ARCHIVE.zip|token USER_ID TENANT_ID")
	}
	base := strings.TrimRight(os.Getenv("API_URL"), "/")
	if base == "" {
		base = "http://localhost:8080"
	}
	token := os.Getenv("EXPORT_TOKEN")
	if token == "" {
		return fmt.Errorf("set EXPORT_TOKEN to a tenant administrator token")
	}
	method, path := "GET", "/api/v1/exports"
	var body io.Reader
	switch args[0] {
	case "request":
		method = "POST"
		body = strings.NewReader(`{"format":"zip","datasets":["all"]}`)
	case "list":
	case "status":
		if len(args) != 2 || !exportIDPattern.MatchString(args[1]) {
			return fmt.Errorf("usage: exportctl status ID")
		}
		path += "/" + args[1]
	case "download":
		if len(args) != 2 || !exportIDPattern.MatchString(args[1]) {
			return fmt.Errorf("usage: exportctl download ID")
		}
		path += "/" + args[1] + "/download"
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
	req, e := http.NewRequest(method, base+path, body)
	if e != nil {
		return e
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", fmt.Sprintf("cli-%d", time.Now().UnixNano()))
	}
	resp, e := http.DefaultClient.Do(req)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("API returned %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	if args[0] != "download" {
		_, e = io.Copy(os.Stdout, io.LimitReader(resp.Body, 1<<20))
		return e
	}
	var result struct {
		URL string `json:"url"`
	}
	if e = json.NewDecoder(resp.Body).Decode(&result); e != nil {
		return e
	}
	client := http.Client{Timeout: 2 * time.Minute}
	file, e := client.Get(result.URL)
	if e != nil {
		return e
	}
	defer file.Body.Close()
	if file.StatusCode/100 != 2 {
		return fmt.Errorf("object storage returned %s", file.Status)
	}
	out := filepath.Clean(args[1] + ".zip")
	f, e := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(f, file.Body)
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	fmt.Println(out)
	return nil
}
