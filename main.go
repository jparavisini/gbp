// gbp is a minimal CLI for the Google Business Profile APIs.
//
// Business Profile does not support service accounts, so authentication is a
// user OAuth refresh token. Run `gbp login` once with a Google account that
// owns or manages the profiles; it prints an authorized-user JSON credential
// that every other command reads.
//
// Credentials are resolved in order:
//  1. GBP_CREDENTIALS_JSON: inline authorized-user JSON (for CI secrets)
//  2. GBP_CREDENTIALS: path to an authorized-user JSON file
//
// All commands print the raw API JSON response to stdout. Errors go to
// stderr with a non-zero exit.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const scope = "https://www.googleapis.com/auth/business.manage"

// Every field of a Business Information Location. The API requires a read
// mask, so the default is the whole resource.
const allLocationFields = "name,languageCode,storeCode,title,phoneNumbers,categories,storefrontAddress,websiteUri,regularHours,specialHours,serviceArea,labels,adWordsLocationExtensions,latlng,openInfo,metadata,profile,relationshipData,moreHours,serviceItems"

const allDailyMetrics = "BUSINESS_IMPRESSIONS_DESKTOP_MAPS,BUSINESS_IMPRESSIONS_DESKTOP_SEARCH,BUSINESS_IMPRESSIONS_MOBILE_MAPS,BUSINESS_IMPRESSIONS_MOBILE_SEARCH,BUSINESS_CONVERSATIONS,BUSINESS_DIRECTION_REQUESTS,CALL_CLICKS,WEBSITE_CLICKS,BUSINESS_BOOKINGS,BUSINESS_FOOD_ORDERS,BUSINESS_FOOD_MENU_CLICKS"

// version is set at release time via -ldflags "-X main.version=...".
// For `go install` builds it falls back to the module version Go embeds.
var version = "dev"

func resolveVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return version
}

// Business Profile is split across several API hosts. Reviews, posts, and
// media still live only on the legacy v4 API.
//
// Swapped out in tests.
var (
	accountsAPI                = "https://mybusinessaccountmanagement.googleapis.com"
	informationAPI             = "https://mybusinessbusinessinformation.googleapis.com"
	performanceAPI             = "https://businessprofileperformance.googleapis.com"
	verificationsAPI           = "https://mybusinessverifications.googleapis.com"
	legacyAPI                  = "https://mybusiness.googleapis.com"
	newClient                  = credentialClient
	stdin            io.Reader = os.Stdin
	stdout           io.Writer = os.Stdout
	stderr           io.Writer = os.Stderr
)

const usageText = `gbp: Google Business Profile CLI

Usage:
  gbp login <client-secret.json>
  gbp accounts list [paging flags]
  gbp locations list <account> [--read-mask f1,f2] [--filter expr] [--order-by expr] [paging flags]
  gbp locations get <location> [--read-mask f1,f2]
  gbp locations patch <location> --update-mask f1,f2 [--validate-only] < location.json
  gbp reviews list <location> [--order-by expr] [paging flags]
  gbp reviews reply <review> <comment>
  gbp reviews delete-reply <review>
  gbp posts list <location> [paging flags]
  gbp posts create <location> < post.json
  gbp posts delete <post>
  gbp media list <location> [paging flags]
  gbp performance <location> --start YYYY-MM-DD --end YYYY-MM-DD [--metrics m1,m2]
  gbp keywords <location> --start YYYY-MM --end YYYY-MM [paging flags]
  gbp verification <location>
  gbp version

Resources are named exactly as the API returns them: an account is
"accounts/123", a location is "accounts/123/locations/456", a review is
"accounts/123/locations/456/reviews/abc", a post is
"accounts/123/locations/456/localPosts/789". "locations list" returns bare
"locations/456" names; prefix them with the account. Commands other than
reviews, posts, and media also accept the bare form.

Paging flags:
  --page-size    results per page (API default and max vary by resource)
  --page-token   nextPageToken from the previous response

Flags:
  --read-mask    location fields to return (default: all)
  --update-mask  location fields to overwrite from the stdin JSON (required)
  --filter       locations filter, e.g. 'title="Acme"' or 'storeCode="NYC-1"'
  --order-by     locations: title|storeCode [desc]
                 reviews: rating|rating desc|updateTime desc
  --metrics      comma-separated daily metrics (default: all), e.g.
                 WEBSITE_CLICKS,CALL_CLICKS,BUSINESS_DIRECTION_REQUESTS

Auth (authorized-user JSON, printed by "gbp login"):
  GBP_CREDENTIALS_JSON    inline JSON (for CI secrets), or
  GBP_CREDENTIALS         path to JSON file

Output: raw API JSON on stdout. Errors on stderr, exit code 1.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	ctx := context.Background()
	var err error
	switch os.Args[1] {
	case "login":
		err = cmdLogin(ctx, os.Args[2:])
	case "accounts":
		err = cmdAccounts(ctx, os.Args[2:])
	case "locations":
		err = cmdLocations(ctx, os.Args[2:])
	case "reviews":
		err = cmdReviews(ctx, os.Args[2:])
	case "posts":
		err = cmdPosts(ctx, os.Args[2:])
	case "media":
		err = cmdMedia(ctx, os.Args[2:])
	case "performance":
		err = cmdPerformance(ctx, os.Args[2:])
	case "keywords":
		err = cmdKeywords(ctx, os.Args[2:])
	case "verification":
		err = cmdVerification(ctx, os.Args[2:])
	case "version", "--version":
		fmt.Println(resolveVersion())
	case "help", "-h", "--help":
		fmt.Print(usageText)
	default:
		fmt.Fprintf(os.Stderr, "gbp: unknown command %q\n\n%s", os.Args[1], usageText)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gbp:", err)
		os.Exit(1)
	}
}

// cmdLogin runs the OAuth loopback flow for a Desktop-app client and prints
// the resulting authorized-user credential to stdout.
func cmdLogin(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gbp login <client-secret.json>")
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("reading client secret: %w", err)
	}
	conf, err := google.ConfigFromJSON(data, scope)
	if err != nil {
		return fmt.Errorf("parsing client secret (want a Desktop app OAuth client): %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer ln.Close()
	conf.RedirectURL = "http://" + ln.Addr().String()

	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	state := hex.EncodeToString(nonce)

	type result struct {
		code string
		err  error
	}
	done := make(chan result, 1)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Browsers also ask for /favicon.ico; only "/" is the callback.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		var res result
		switch {
		case q.Get("state") != state:
			res.err = fmt.Errorf("login: callback state mismatch")
		case q.Get("error") != "":
			res.err = fmt.Errorf("login: authorization denied: %s", q.Get("error"))
		default:
			res.code = q.Get("code")
		}
		if res.err != nil {
			http.Error(w, res.err.Error(), http.StatusBadRequest)
		} else {
			io.WriteString(w, "gbp: authorized. You can close this tab.\n")
		}
		select {
		case done <- res:
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	// prompt=consent forces Google to issue a refresh token even if this
	// account has authorized the client before.
	authURL := conf.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.SetAuthURLParam("prompt", "consent"))
	fmt.Fprintf(stderr, "gbp: open this URL and sign in with the Google account that manages the profiles:\n\n%s\n\n", authURL)

	var res result
	select {
	case res = <-done:
	case <-time.After(5 * time.Minute):
		return fmt.Errorf("login: timed out waiting for the browser callback")
	}
	if res.err != nil {
		return res.err
	}
	tok, err := conf.Exchange(ctx, res.code)
	if err != nil {
		return fmt.Errorf("login: exchanging code: %w", err)
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("login: Google returned no refresh token")
	}
	out, err := json.MarshalIndent(map[string]string{
		"type":          "authorized_user",
		"client_id":     conf.ClientID,
		"client_secret": conf.ClientSecret,
		"refresh_token": tok.RefreshToken,
	}, "", "  ")
	if err != nil {
		return err
	}
	return emit(out)
}

// pageFlags registers --page-size and --page-token on fs and returns a
// function that copies the ones the user set into q.
func pageFlags(fs *flag.FlagSet) func(q url.Values) {
	size := fs.Int("page-size", 0, "results per page")
	token := fs.String("page-token", "", "nextPageToken from the previous response")
	return func(q url.Values) {
		if *size > 0 {
			q.Set("pageSize", strconv.Itoa(*size))
		}
		if *token != "" {
			q.Set("pageToken", *token)
		}
	}
}

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

// bareLocation returns the "locations/456" form the v1 APIs take, from either
// that form or the account-qualified "accounts/123/locations/456".
func bareLocation(name string) (string, error) {
	parts := strings.Split(name, "/")
	if len(parts) == 2 && parts[0] == "locations" && parts[1] != "" {
		return name, nil
	}
	if err := childOf(name, "locations"); err != nil {
		return "", fmt.Errorf("bad location %q, want accounts/*/locations/* or locations/*", name)
	}
	return "locations/" + parts[3], nil
}

// childOf checks that name is an account-qualified resource of the given
// shape, e.g. childOf(name, "locations", "reviews") accepts
// "accounts/1/locations/2/reviews/3". The legacy v4 API takes no other form.
func childOf(name string, collections ...string) error {
	parts := strings.Split(name, "/")
	want := append([]string{"accounts"}, collections...)
	ok := len(parts) == 2*len(want)
	for i := 0; ok && i < len(want); i++ {
		ok = parts[2*i] == want[i] && parts[2*i+1] != ""
	}
	if !ok {
		var shape []string
		for _, c := range want {
			shape = append(shape, c, "*")
		}
		return fmt.Errorf("bad resource name %q, want %s", name, strings.Join(shape, "/"))
	}
	return nil
}

func cmdAccounts(ctx context.Context, args []string) error {
	if len(args) < 1 || args[0] != "list" {
		return fmt.Errorf("usage: gbp accounts list [paging flags]")
	}
	fs := newFlagSet("accounts list")
	page := pageFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	q := url.Values{}
	page(q)
	return get(ctx, accountsAPI+"/v1/accounts", q)
}

func cmdLocations(ctx context.Context, args []string) error {
	usage := fmt.Errorf("usage: gbp locations list <account> | get <location> | patch <location> --update-mask f1,f2")
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return usage
	}
	sub, name := args[0], args[1]
	fs := newFlagSet("locations " + sub)
	q := url.Values{}

	switch sub {
	case "list":
		readMask := fs.String("read-mask", allLocationFields, "fields to return")
		filter := fs.String("filter", "", "filter expression")
		orderBy := fs.String("order-by", "", "title|storeCode [desc]")
		page := pageFlags(fs)
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if err := childOf(name); err != nil {
			return err
		}
		q.Set("readMask", *readMask)
		if *filter != "" {
			q.Set("filter", *filter)
		}
		if *orderBy != "" {
			q.Set("orderBy", *orderBy)
		}
		page(q)
		return get(ctx, informationAPI+"/v1/"+name+"/locations", q)

	case "get":
		readMask := fs.String("read-mask", allLocationFields, "fields to return")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		location, err := bareLocation(name)
		if err != nil {
			return err
		}
		q.Set("readMask", *readMask)
		return get(ctx, informationAPI+"/v1/"+location, q)

	case "patch":
		updateMask := fs.String("update-mask", "", "fields to overwrite (required)")
		validateOnly := fs.Bool("validate-only", false, "validate without saving")
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		location, err := bareLocation(name)
		if err != nil {
			return err
		}
		if *updateMask == "" {
			return fmt.Errorf("locations patch: --update-mask is required")
		}
		body, err := stdinJSON()
		if err != nil {
			return err
		}
		q.Set("updateMask", *updateMask)
		if *validateOnly {
			q.Set("validateOnly", "true")
		}
		out, err := call(ctx, http.MethodPatch, informationAPI+"/v1/"+location+"?"+q.Encode(), body)
		if err != nil {
			return err
		}
		return emit(out)

	default:
		return usage
	}
}

func cmdReviews(ctx context.Context, args []string) error {
	usage := fmt.Errorf("usage: gbp reviews list <location> | reply <review> <comment> | delete-reply <review>")
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return usage
	}
	sub, name := args[0], args[1]
	switch sub {
	case "list":
		fs := newFlagSet("reviews list")
		orderBy := fs.String("order-by", "", "rating|rating desc|updateTime desc")
		page := pageFlags(fs)
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if err := childOf(name, "locations"); err != nil {
			return err
		}
		q := url.Values{}
		if *orderBy != "" {
			q.Set("orderBy", *orderBy)
		}
		page(q)
		return get(ctx, legacyAPI+"/v4/"+name+"/reviews", q)

	case "reply":
		if len(args) != 3 {
			return usage
		}
		if err := childOf(name, "locations", "reviews"); err != nil {
			return err
		}
		out, err := call(ctx, http.MethodPut, legacyAPI+"/v4/"+name+"/reply", map[string]string{"comment": args[2]})
		if err != nil {
			return err
		}
		return emit(out)

	case "delete-reply":
		if len(args) != 2 {
			return usage
		}
		if err := childOf(name, "locations", "reviews"); err != nil {
			return err
		}
		if _, err := call(ctx, http.MethodDelete, legacyAPI+"/v4/"+name+"/reply", nil); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "gbp: deleted reply on %s\n", name)
		return nil

	default:
		return usage
	}
}

func cmdPosts(ctx context.Context, args []string) error {
	usage := fmt.Errorf("usage: gbp posts list <location> | create <location> | delete <post>")
	if len(args) < 2 || strings.HasPrefix(args[1], "-") {
		return usage
	}
	sub, name := args[0], args[1]
	switch sub {
	case "list":
		fs := newFlagSet("posts list")
		page := pageFlags(fs)
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		if err := childOf(name, "locations"); err != nil {
			return err
		}
		q := url.Values{}
		page(q)
		return get(ctx, legacyAPI+"/v4/"+name+"/localPosts", q)

	case "create":
		if len(args) != 2 {
			return usage
		}
		if err := childOf(name, "locations"); err != nil {
			return err
		}
		body, err := stdinJSON()
		if err != nil {
			return err
		}
		out, err := call(ctx, http.MethodPost, legacyAPI+"/v4/"+name+"/localPosts", body)
		if err != nil {
			return err
		}
		return emit(out)

	case "delete":
		if len(args) != 2 {
			return usage
		}
		if err := childOf(name, "locations", "localPosts"); err != nil {
			return err
		}
		if _, err := call(ctx, http.MethodDelete, legacyAPI+"/v4/"+name, nil); err != nil {
			return err
		}
		fmt.Fprintf(stderr, "gbp: deleted %s\n", name)
		return nil

	default:
		return usage
	}
}

func cmdMedia(ctx context.Context, args []string) error {
	if len(args) < 2 || args[0] != "list" || strings.HasPrefix(args[1], "-") {
		return fmt.Errorf("usage: gbp media list <location> [paging flags]")
	}
	fs := newFlagSet("media list")
	page := pageFlags(fs)
	if err := fs.Parse(args[2:]); err != nil {
		return err
	}
	if err := childOf(args[1], "locations"); err != nil {
		return err
	}
	q := url.Values{}
	page(q)
	return get(ctx, legacyAPI+"/v4/"+args[1]+"/media", q)
}

// setDate writes a date as the year/month[/day] query fields the Performance
// API takes, e.g. prefix "dailyRange.start_date" and value "2026-06-01".
func setDate(q url.Values, prefix, layout, value string) error {
	t, err := time.Parse(layout, value)
	if err != nil {
		return fmt.Errorf("bad date %q, want %s", value, strings.NewReplacer("2006", "YYYY", "01", "MM", "02", "DD").Replace(layout))
	}
	q.Set(prefix+".year", strconv.Itoa(t.Year()))
	q.Set(prefix+".month", strconv.Itoa(int(t.Month())))
	if strings.Contains(layout, "02") {
		q.Set(prefix+".day", strconv.Itoa(t.Day()))
	}
	return nil
}

func cmdPerformance(ctx context.Context, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: gbp performance <location> --start YYYY-MM-DD --end YYYY-MM-DD [--metrics m1,m2]")
	}
	fs := newFlagSet("performance")
	start := fs.String("start", "", "start date YYYY-MM-DD (required)")
	end := fs.String("end", "", "end date YYYY-MM-DD (required)")
	metrics := fs.String("metrics", allDailyMetrics, "comma-separated daily metrics")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	location, err := bareLocation(args[0])
	if err != nil {
		return err
	}
	if *start == "" || *end == "" {
		return fmt.Errorf("performance: --start and --end are required")
	}
	q := url.Values{"dailyMetrics": strings.Split(*metrics, ",")}
	if err := setDate(q, "dailyRange.start_date", "2006-01-02", *start); err != nil {
		return err
	}
	if err := setDate(q, "dailyRange.end_date", "2006-01-02", *end); err != nil {
		return err
	}
	return get(ctx, performanceAPI+"/v1/"+location+":fetchMultiDailyMetricsTimeSeries", q)
}

func cmdKeywords(ctx context.Context, args []string) error {
	if len(args) < 1 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: gbp keywords <location> --start YYYY-MM --end YYYY-MM [paging flags]")
	}
	fs := newFlagSet("keywords")
	start := fs.String("start", "", "start month YYYY-MM (required)")
	end := fs.String("end", "", "end month YYYY-MM (required)")
	page := pageFlags(fs)
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	location, err := bareLocation(args[0])
	if err != nil {
		return err
	}
	if *start == "" || *end == "" {
		return fmt.Errorf("keywords: --start and --end are required")
	}
	q := url.Values{}
	if err := setDate(q, "monthlyRange.start_month", "2006-01", *start); err != nil {
		return err
	}
	if err := setDate(q, "monthlyRange.end_month", "2006-01", *end); err != nil {
		return err
	}
	page(q)
	return get(ctx, performanceAPI+"/v1/"+location+"/searchkeywords/impressions/monthly", q)
}

func cmdVerification(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: gbp verification <location>")
	}
	location, err := bareLocation(args[0])
	if err != nil {
		return err
	}
	return get(ctx, verificationsAPI+"/v1/"+location+"/VoiceOfMerchantState", nil)
}

func credentialClient(ctx context.Context) (*http.Client, error) {
	var data []byte
	if inline := os.Getenv("GBP_CREDENTIALS_JSON"); inline != "" {
		data = []byte(inline)
	} else {
		path := os.Getenv("GBP_CREDENTIALS")
		if path == "" {
			return nil, fmt.Errorf("no credentials: run `gbp login`, then set GBP_CREDENTIALS_JSON (inline authorized-user JSON) or GBP_CREDENTIALS (path to JSON file)")
		}
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading credentials: %w", err)
		}
	}
	creds, err := google.CredentialsFromJSON(ctx, data, scope)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials: %w", err)
	}
	return oauth2.NewClient(ctx, creds.TokenSource), nil
}

// stdinJSON reads the request body for write commands.
func stdinJSON() (json.RawMessage, error) {
	data, err := io.ReadAll(stdin)
	if err != nil {
		return nil, err
	}
	if !json.Valid(data) {
		return nil, fmt.Errorf("stdin is not valid JSON")
	}
	return data, nil
}

func get(ctx context.Context, endpoint string, q url.Values) error {
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	out, err := call(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	return emit(out)
}

func call(ctx context.Context, method, endpoint string, body any) ([]byte, error) {
	c, err := newClient(ctx)
	if err != nil {
		return nil, err
	}
	var payload io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, payload)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s %s: %s\n%s", method, req.URL.Path, resp.Status, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func emit(out []byte) error {
	out = bytes.TrimSpace(out)
	if len(out) == 0 {
		return nil
	}
	_, err := stdout.Write(append(out, '\n'))
	return err
}
