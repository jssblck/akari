package storetest

import (
	"net/url"
	"strings"
	"testing"
)

// TestSlugify pins the database-name slug rules: lower-cased, every character
// outside [a-z0-9] becomes an underscore, and runs are trimmed at the ends so the
// slug never starts or ends with one. Subtest names (with slashes) and all-symbol
// or non-ASCII inputs are the cases that actually reach this from t.Name().
func TestSlugify(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"TestFoo":                   "testfoo",
		"GraceHopper":               "gracehopper",
		"TestFoo/sub case":          "testfoo_sub_case",
		"a/b/c":                     "a_b_c",
		"Ada_Lovelace-1843":         "ada_lovelace_1843",
		"  leading and trailing  ":  "leading_and_trailing",
		"!!!":                       "",
		"café":                      "caf",
		"Anna Winlock":              "anna_winlock",
		"TestX/uppercase_AND_punct": "testx_uppercase_and_punct",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestDBName covers the composition and the 63-byte identifier bound: short names
// pass through whole, and a name long enough to overflow is truncated while the
// prefix and the full random suffix are always kept, so uniqueness survives.
func TestDBName(t *testing.T) {
	t.Parallel()

	if got, want := dbName("TestFoo", "deadbeef"), "akari_test_testfoo_deadbeef"; got != want {
		t.Errorf("dbName short = %q, want %q", got, want)
	}

	// A name far past the limit, with a full 16-hex suffix.
	const suffix = "0123456789abcdef"
	long := dbName(strings.Repeat("grace", 40), suffix)
	if len(long) != 63 {
		t.Errorf("dbName long len = %d, want exactly 63 (filled to the bound)", len(long))
	}
	if !strings.HasPrefix(long, "akari_test_") {
		t.Errorf("dbName long = %q, want the akari_test_ prefix", long)
	}
	if !strings.HasSuffix(long, "_"+suffix) {
		t.Errorf("dbName long = %q, want the random suffix kept", long)
	}

	// Every character must be legal in an unquoted Postgres identifier, and the
	// first must be a letter, so the name needs no quoting beyond our own.
	for i, r := range long {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_'
		if !ok {
			t.Fatalf("dbName produced an illegal identifier char %q at %d in %q", r, i, long)
		}
	}
	if first := long[0]; !(first >= 'a' && first <= 'z') {
		t.Errorf("dbName must start with a letter, got %q", first)
	}
}

// TestMaintenanceURL confirms the URL is retargeted at the postgres maintenance
// database while host, credentials, and query options are preserved, and that a
// malformed URL surfaces an error rather than a silent default.
func TestMaintenanceURL(t *testing.T) {
	t.Parallel()

	got, err := maintenanceURL("postgres://akari:secret@db.example:55433/akari?sslmode=disable")
	if err != nil {
		t.Fatalf("maintenanceURL: %v", err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if u.Path != "/postgres" {
		t.Errorf("path = %q, want /postgres", u.Path)
	}
	if u.Host != "db.example:55433" {
		t.Errorf("host = %q, want db.example:55433", u.Host)
	}
	if pw, _ := u.User.Password(); u.User.Username() != "akari" || pw != "secret" {
		t.Errorf("credentials not preserved: %v", u.User)
	}
	if u.Query().Get("sslmode") != "disable" {
		t.Errorf("sslmode option dropped: %q", u.RawQuery)
	}

	if _, err := maintenanceURL("postgres://host:notaport/db"); err == nil {
		t.Error("maintenanceURL on a malformed URL should error")
	}
}

// TestTestDBURL confirms the base URL is retargeted at the per-test database, the
// existing query options survive, and the connection pool is capped so a fully
// parallel suite stays under the server's connection limit.
func TestTestDBURL(t *testing.T) {
	t.Parallel()

	got := testDBURL("postgres://akari:akari@127.0.0.1:55433/akari?sslmode=disable", "akari_test_grace_01")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if u.Path != "/akari_test_grace_01" {
		t.Errorf("path = %q, want /akari_test_grace_01", u.Path)
	}
	q := u.Query()
	if q.Get("sslmode") != "disable" {
		t.Errorf("existing sslmode option dropped: %q", u.RawQuery)
	}
	if q.Get("pool_max_conns") != "4" {
		t.Errorf("pool_max_conns = %q, want 4", q.Get("pool_max_conns"))
	}
}
