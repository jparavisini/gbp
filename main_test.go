package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	testAccount  = "accounts/1"
	testLocation = "accounts/1/locations/2"
	testReview   = "accounts/1/locations/2/reviews/3"
	testPost     = "accounts/1/locations/2/localPosts/4"
)

// withServer points every API host at a test server with no real auth and
// captures stdout. It restores everything on cleanup.
func withServer(t *testing.T, handler http.HandlerFunc) *bytes.Buffer {
	t.Helper()
	srv := httptest.NewServer(handler)
	hosts := []*string{&accountsAPI, &informationAPI, &performanceAPI, &verificationsAPI, &legacyAPI}
	origHosts := make([]string, len(hosts))
	for i, h := range hosts {
		origHosts[i], *h = *h, srv.URL
	}
	origClient, origStdout, origStderr := newClient, stdout, stderr
	buf := &bytes.Buffer{}
	newClient = func(ctx context.Context) (*http.Client, error) { return srv.Client(), nil }
	stdout, stderr = buf, io.Discard
	t.Cleanup(func() {
		srv.Close()
		for i, h := range hosts {
			*h = origHosts[i]
		}
		newClient, stdout, stderr = origClient, origStdout, origStderr
	})
	return buf
}

func withStdin(t *testing.T, s string) {
	t.Helper()
	orig := stdin
	stdin = strings.NewReader(s)
	t.Cleanup(func() { stdin = orig })
}

func TestAccountsList(t *testing.T) {
	var got *http.Request
	buf := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		io.WriteString(w, `{"accounts":[{"name":"accounts/1"}]}`)
	})
	if err := cmdAccounts(context.Background(), []string{"list", "--page-size", "5", "--page-token", "tok"}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodGet || got.URL.Path != "/v1/accounts" {
		t.Errorf("got %s %s", got.Method, got.URL.Path)
	}
	if q := got.URL.Query(); q.Get("pageSize") != "5" || q.Get("pageToken") != "tok" {
		t.Errorf("query = %v", q)
	}
	if !strings.Contains(buf.String(), "accounts/1") {
		t.Errorf("stdout = %q, want accounts JSON", buf.String())
	}
}

func TestLocationsListDefaultsReadMask(t *testing.T) {
	var got *http.Request
	withServer(t, func(w http.ResponseWriter, r *http.Request) { got = r })
	err := cmdLocations(context.Background(), []string{"list", testAccount, "--filter", `title="Acme"`})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/v1/accounts/1/locations" {
		t.Errorf("path = %s", got.URL.Path)
	}
	q := got.URL.Query()
	if q.Get("readMask") != allLocationFields || q.Get("filter") != `title="Acme"` {
		t.Errorf("query = %v", q)
	}
	if q.Has("pageSize") || q.Has("pageToken") || q.Has("orderBy") {
		t.Errorf("unset flags leaked into query: %v", q)
	}
}

func TestLocationsGetAcceptsBothNameForms(t *testing.T) {
	for _, name := range []string{testLocation, "locations/2"} {
		var got *http.Request
		withServer(t, func(w http.ResponseWriter, r *http.Request) { got = r })
		if err := cmdLocations(context.Background(), []string{"get", name, "--read-mask", "title"}); err != nil {
			t.Fatal(err)
		}
		if got.URL.Path != "/v1/locations/2" || got.URL.Query().Get("readMask") != "title" {
			t.Errorf("%s: got %s", name, got.URL)
		}
	}
}

func TestLocationsPatch(t *testing.T) {
	var got *http.Request
	var body map[string]any
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"name":"locations/2"}`)
	})
	withStdin(t, `{"websiteUri":"https://example.com/"}`)
	err := cmdLocations(context.Background(), []string{
		"patch", testLocation, "--update-mask", "websiteUri", "--validate-only",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPatch || got.URL.Path != "/v1/locations/2" {
		t.Errorf("got %s %s", got.Method, got.URL.Path)
	}
	if q := got.URL.Query(); q.Get("updateMask") != "websiteUri" || q.Get("validateOnly") != "true" {
		t.Errorf("query = %v", q)
	}
	if body["websiteUri"] != "https://example.com/" {
		t.Errorf("body = %v, want stdin JSON passed through", body)
	}
}

func TestLocationsPatchValidation(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("request sent despite invalid input")
	})
	ctx := context.Background()
	withStdin(t, `{}`)
	if err := cmdLocations(ctx, []string{"patch", testLocation}); err == nil {
		t.Error("missing --update-mask: want error")
	}
	withStdin(t, `not json`)
	if err := cmdLocations(ctx, []string{"patch", testLocation, "--update-mask", "title"}); err == nil {
		t.Error("invalid stdin JSON: want error")
	}
}

func TestReviewsList(t *testing.T) {
	var got *http.Request
	buf := withServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		io.WriteString(w, `{"reviews":[],"averageRating":4.8}`)
	})
	err := cmdReviews(context.Background(), []string{"list", testLocation, "--order-by", "rating desc"})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/v4/accounts/1/locations/2/reviews" || got.URL.Query().Get("orderBy") != "rating desc" {
		t.Errorf("got %s", got.URL)
	}
	if !strings.Contains(buf.String(), "averageRating") {
		t.Errorf("stdout = %q", buf.String())
	}
}

func TestReviewsReply(t *testing.T) {
	var got *http.Request
	var body map[string]any
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		json.NewDecoder(r.Body).Decode(&body)
		io.WriteString(w, `{"comment":"Thanks!"}`)
	})
	if err := cmdReviews(context.Background(), []string{"reply", testReview, "Thanks!"}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPut || got.URL.Path != "/v4/"+testReview+"/reply" {
		t.Errorf("got %s %s", got.Method, got.URL.Path)
	}
	if body["comment"] != "Thanks!" {
		t.Errorf("body = %v", body)
	}
}

func TestReviewsDeleteReply(t *testing.T) {
	var got *http.Request
	buf := withServer(t, func(w http.ResponseWriter, r *http.Request) { got = r })
	if err := cmdReviews(context.Background(), []string{"delete-reply", testReview}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodDelete || got.URL.Path != "/v4/"+testReview+"/reply" {
		t.Errorf("got %s %s", got.Method, got.URL.Path)
	}
	if buf.Len() != 0 {
		t.Errorf("stdout = %q, want empty on delete", buf.String())
	}
}

func TestPostsCreateAndDelete(t *testing.T) {
	var got *http.Request
	var body map[string]any
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r
		json.NewDecoder(r.Body).Decode(&body)
	})
	ctx := context.Background()
	withStdin(t, `{"summary":"Open late Friday","topicType":"STANDARD"}`)
	if err := cmdPosts(ctx, []string{"create", testLocation}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/v4/"+testLocation+"/localPosts" || body["topicType"] != "STANDARD" {
		t.Errorf("create: got %s %s body %v", got.Method, got.URL.Path, body)
	}
	if err := cmdPosts(ctx, []string{"delete", testPost}); err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodDelete || got.URL.Path != "/v4/"+testPost {
		t.Errorf("delete: got %s %s", got.Method, got.URL.Path)
	}
}

func TestMediaList(t *testing.T) {
	var got *http.Request
	withServer(t, func(w http.ResponseWriter, r *http.Request) { got = r })
	if err := cmdMedia(context.Background(), []string{"list", testLocation}); err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/v4/"+testLocation+"/media" {
		t.Errorf("path = %s", got.URL.Path)
	}
}

func TestPerformanceQuery(t *testing.T) {
	var got *http.Request
	withServer(t, func(w http.ResponseWriter, r *http.Request) { got = r })
	err := cmdPerformance(context.Background(), []string{
		testLocation, "--start", "2026-06-01", "--end", "2026-06-30",
		"--metrics", "WEBSITE_CLICKS,CALL_CLICKS",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/v1/locations/2:fetchMultiDailyMetricsTimeSeries" {
		t.Errorf("path = %s", got.URL.Path)
	}
	q := got.URL.Query()
	if m := q["dailyMetrics"]; len(m) != 2 || m[0] != "WEBSITE_CLICKS" || m[1] != "CALL_CLICKS" {
		t.Errorf("dailyMetrics = %v, want one repeated param per metric", m)
	}
	for k, want := range map[string]string{
		"dailyRange.start_date.year": "2026", "dailyRange.start_date.month": "6", "dailyRange.start_date.day": "1",
		"dailyRange.end_date.year": "2026", "dailyRange.end_date.month": "6", "dailyRange.end_date.day": "30",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
}

func TestKeywordsQuery(t *testing.T) {
	var got *http.Request
	withServer(t, func(w http.ResponseWriter, r *http.Request) { got = r })
	err := cmdKeywords(context.Background(), []string{"locations/2", "--start", "2026-01", "--end", "2026-03"})
	if err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/v1/locations/2/searchkeywords/impressions/monthly" {
		t.Errorf("path = %s", got.URL.Path)
	}
	q := got.URL.Query()
	if q.Get("monthlyRange.start_month.month") != "1" || q.Get("monthlyRange.end_month.month") != "3" || q.Get("monthlyRange.end_month.year") != "2026" {
		t.Errorf("query = %v", q)
	}
	if q.Has("monthlyRange.start_month.day") {
		t.Errorf("monthly range must not carry a day: %v", q)
	}
}

func TestVerification(t *testing.T) {
	var got *http.Request
	withServer(t, func(w http.ResponseWriter, r *http.Request) { got = r })
	if err := cmdVerification(context.Background(), []string{testLocation}); err != nil {
		t.Fatal(err)
	}
	if got.URL.Path != "/v1/locations/2/VoiceOfMerchantState" {
		t.Errorf("path = %s", got.URL.Path)
	}
}

func TestInputErrorsSendNoRequest(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request sent despite invalid input: %s", r.URL)
	})
	ctx := context.Background()
	cases := map[string]func() error{
		"accounts no subcommand":      func() error { return cmdAccounts(ctx, nil) },
		"locations list bad account":  func() error { return cmdLocations(ctx, []string{"list", "locations/2"}) },
		"locations get bad name":      func() error { return cmdLocations(ctx, []string{"get", "accounts/1"}) },
		"locations get empty id":      func() error { return cmdLocations(ctx, []string{"get", "locations/"}) },
		"locations bogus subcommand":  func() error { return cmdLocations(ctx, []string{"bogus", testLocation}) },
		"reviews list bare location":  func() error { return cmdReviews(ctx, []string{"list", "locations/2"}) },
		"reviews reply to a location": func() error { return cmdReviews(ctx, []string{"reply", testLocation, "hi"}) },
		"reviews reply no comment":    func() error { return cmdReviews(ctx, []string{"reply", testReview}) },
		"posts delete a location":     func() error { return cmdPosts(ctx, []string{"delete", testLocation}) },
		"media missing location":      func() error { return cmdMedia(ctx, []string{"list"}) },
		"performance missing dates":   func() error { return cmdPerformance(ctx, []string{testLocation}) },
		"performance bad date": func() error {
			return cmdPerformance(ctx, []string{testLocation, "--start", "June 1", "--end", "2026-06-30"})
		},
		"keywords day-precision date": func() error {
			return cmdKeywords(ctx, []string{testLocation, "--start", "2026-01-01", "--end", "2026-03"})
		},
		"verification no args": func() error { return cmdVerification(ctx, nil) },
	}
	for name, run := range cases {
		if err := run(); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

func TestAPIErrorSurfaced(t *testing.T) {
	withServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, `{"error":{"message":"Quota exceeded for quota metric 'Requests'"}}`)
	})
	err := cmdAccounts(context.Background(), []string{"list"})
	if err == nil {
		t.Fatal("want error on 403")
	}
	for _, want := range []string{"403", "Quota exceeded"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestCredentialResolution(t *testing.T) {
	ctx := context.Background()
	t.Setenv("GBP_CREDENTIALS_JSON", "")
	t.Setenv("GBP_CREDENTIALS", "")
	if _, err := credentialClient(ctx); err == nil || !strings.Contains(err.Error(), "no credentials") {
		t.Errorf("unset env: err = %v, want no-credentials error", err)
	}
	t.Setenv("GBP_CREDENTIALS_JSON", "{}")
	if _, err := credentialClient(ctx); err == nil || !strings.Contains(err.Error(), "parsing credentials") {
		t.Errorf("bad JSON: err = %v, want parse error", err)
	}
	t.Setenv("GBP_CREDENTIALS_JSON", "")
	t.Setenv("GBP_CREDENTIALS", "/nonexistent/creds.json")
	if _, err := credentialClient(ctx); err == nil || !strings.Contains(err.Error(), "reading credentials") {
		t.Errorf("missing file: err = %v, want read error", err)
	}
	t.Setenv("GBP_CREDENTIALS_JSON", `{"type":"authorized_user","client_id":"id","client_secret":"secret","refresh_token":"rt"}`)
	if _, err := credentialClient(ctx); err != nil {
		t.Errorf("authorized_user JSON, the format login prints: err = %v", err)
	}
}

// TestLogin plays the browser: it reads the auth URL from stderr, follows its
// redirect_uri with a code, and checks the credential file login writes.
func TestLogin(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		if r.Form.Get("code") != "the-code" {
			t.Errorf("token exchange code = %q", r.Form.Get("code"))
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"access_token":"at","refresh_token":"the-refresh-token","token_type":"Bearer","expires_in":3600}`)
	}))
	defer tokenSrv.Close()

	secret := filepath.Join(t.TempDir(), "client.json")
	os.WriteFile(secret, []byte(fmt.Sprintf(
		`{"installed":{"client_id":"id","client_secret":"secret","auth_uri":"https://auth.invalid/auth","token_uri":%q,"redirect_uris":["http://localhost"]}}`,
		tokenSrv.URL)), 0o600)

	credDir := filepath.Join(t.TempDir(), "gbp")
	credPath := filepath.Join(credDir, "credentials.json")

	origStderr := stderr
	t.Cleanup(func() { stderr = origStderr })

	// startLogin runs cmdLogin and returns the auth URL it printed.
	startLogin := func() (url.Values, chan error) {
		t.Helper()
		pr, pw := io.Pipe()
		stderr = pw
		done := make(chan error, 1)
		go func() { done <- cmdLogin(context.Background(), []string{secret, credPath}) }()
		sc := bufio.NewScanner(pr)
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "https://auth.invalid/auth") {
				authURL, err := url.Parse(sc.Text())
				if err != nil {
					t.Fatal(err)
				}
				go io.Copy(io.Discard, pr)
				return authURL.Query(), done
			}
		}
		t.Fatal("no auth URL on stderr")
		return nil, nil
	}
	callback := func(q url.Values, state string) {
		t.Helper()
		resp, err := http.Get(q.Get("redirect_uri") + "?code=the-code&state=" + state)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credPath, []byte("previous"), 0o600); err != nil {
		t.Fatal(err)
	}
	q, done := startLogin()
	if q.Get("scope") != scope || q.Get("access_type") != "offline" || q.Get("prompt") != "consent" {
		t.Errorf("auth URL query = %v", q)
	}
	callback(q, "forged")
	if err := <-done; err == nil || !strings.Contains(err.Error(), "state mismatch") {
		t.Fatalf("forged state: err = %v, want state mismatch", err)
	}
	if b, _ := os.ReadFile(credPath); string(b) != "previous" {
		t.Errorf("failed login changed the credential file: %q", b)
	}

	q, done = startLogin()
	callback(q, q.Get("state"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	var cred map[string]string
	if err := json.Unmarshal(b, &cred); err != nil {
		t.Fatalf("credential file = %q: %v", b, err)
	}
	if cred["type"] != "authorized_user" || cred["refresh_token"] != "the-refresh-token" || cred["client_id"] != "id" {
		t.Errorf("credential = %v", cred)
	}
	if fi, err := os.Stat(credPath); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("credential file mode = %v, want 0600", fi.Mode().Perm())
	}
	if entries, _ := os.ReadDir(credDir); len(entries) != 1 {
		t.Errorf("credential dir has %d entries, want only credentials.json", len(entries))
	}
}
