package storage

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestVersionQueryCanonicalization(t *testing.T) {
	q := url.Values{}
	q.Set("versionId", "v 1")
	if got := canonicalQuery(q); got != "versionId=v%201" {
		t.Fatalf("canonical query %q", got)
	}
}

func TestObjectKeyCharactersStayInPath(t *testing.T) {
	u, e := parseObjectURL("http://localhost:9000", "bucket", "tenant/a b?#.txt")
	if e != nil {
		t.Fatal(e)
	}
	if u.RawQuery != "" || u.Fragment != "" || u.EscapedPath() != "/bucket/tenant/a%20b%3F%23.txt" {
		t.Fatalf("unsafe URL parsing: %#v", u)
	}
}

func TestPresignGetHasBoundedExpiry(t *testing.T) {
	c := New("http://localhost:9000", "us-east-1", "access", "secret")
	if _, e := c.PresignGet("exports", "a.zip", 8*24*time.Hour); e == nil {
		t.Fatal("expected expiry bound")
	}
	u, e := c.PresignGet("exports", "a.zip", 60*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(u, "X-Amz-Signature=") || !strings.Contains(u, "X-Amz-Expires=60") {
		t.Fatalf("unexpected URL %s", u)
	}
	publicURL, e := c.PresignGetAt("http://downloads.example", "exports", "a.zip", 60*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(publicURL, "http://downloads.example/") {
		t.Fatalf("public endpoint not used: %s", publicURL)
	}
}
