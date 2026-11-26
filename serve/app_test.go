// app_test.go: the browser app and its sign-in
//
// Copyright (c) 2026 AGILira - A. Giordano
// SPDX-License-Identifier: MPL-2.0

package serve

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	agent "github.com/agilira/ringil-agent-sdk"
	"github.com/agilira/ringil-agent-sdk/provider/fake"
)

var appFS = fstest.MapFS{
	"index.html":    {Data: []byte("<html>the app</html>")},
	"assets/app.js": {Data: []byte("console.log(1)")},
}

func appAPI(t *testing.T) *api {
	t.Helper()
	return newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New(fake.Reply("ok"))})}, App: appFS})
}

// raw sends a request with exactly the headers given: no token unless
// asked.
func (a *api) raw(method, path string, headers ...string) *http.Response {
	a.t.Helper()
	req, err := http.NewRequest(method, a.url+path, nil)
	if err != nil {
		a.t.Fatal(err)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i] == "Host" {
			req.Host = headers[i+1]
			continue
		}
		req.Header.Set(headers[i], headers[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	a.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func body(r *http.Response) string {
	b, _ := io.ReadAll(r.Body)
	return string(b)
}

// signedIn signs in with a fresh link and returns the session cookie.
func (a *api) signedIn() string {
	a.t.Helper()
	resp := a.raw("GET", a.srv.SignInPath())
	c := resp.Header.Get("Set-Cookie")
	if resp.StatusCode != http.StatusSeeOther || !strings.Contains(c, "HttpOnly") || !strings.Contains(c, "SameSite=Strict") {
		a.t.Fatalf("sign-in %d %q", resp.StatusCode, c)
	}
	return strings.SplitN(c, ";", 2)[0]
}

// The app's files and its routes are served without a token, with
// headers that keep it out of other pages' frames.
func TestAppFiles(t *testing.T) {
	a := appAPI(t)
	for path, want := range map[string]string{"/": "the app", "/assets/app.js": "console.log", "/chat/abc": "the app"} {
		resp := a.raw("GET", path)
		if got := body(resp); resp.StatusCode != http.StatusOK || !strings.Contains(got, want) {
			t.Errorf("%s: %d %q", path, resp.StatusCode, got)
		}
		if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", path, csp)
		}
	}
	if resp := a.raw("GET", "/", "Host", "evil.example.com"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("another host: %d", resp.StatusCode)
	}
	if resp := a.raw("POST", "/"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST to the app: %d", resp.StatusCode)
	}
}

// A link signs in once, within its time; the session lets the app's own
// page call the API, and signing out ends it.
func TestAppSignIn(t *testing.T) {
	a := appAPI(t)
	link := a.srv.SignInPath()
	if resp := a.raw("GET", link); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("first use %d", resp.StatusCode)
	}
	if resp := a.raw("GET", link); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("second use %d", resp.StatusCode)
	}
	old := a.srv.SignInPath()
	a.srv.now = func() time.Time { return time.Now().Add(codeLife + time.Minute) }
	if resp := a.raw("GET", old); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired %d", resp.StatusCode)
	}
	a.srv.now = time.Now
	if resp := a.raw("GET", "/signin?code=guess"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("guessed %d", resp.StatusCode)
	}

	cookie := a.signedIn()
	if resp := a.raw("GET", "/v1/profiles", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusOK {
		t.Fatalf("signed in: %d %s", resp.StatusCode, body(resp))
	}
	if resp := a.raw("POST", "/v1/signout", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out %d", resp.StatusCode)
	}
	if resp := a.raw("GET", "/v1/profiles", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after sign out: %d", resp.StatusCode)
	}
}

// The cookie is not enough: a request must come from the app's own
// origin. Another port of 127.0.0.1 is the same site for a browser and
// gets the cookie: it is refused.
func TestAppCookieNeedsTheOrigin(t *testing.T) {
	a := appAPI(t)
	cookie := a.signedIn()
	own := "http://" + strings.TrimPrefix(a.url, "http://")
	for name, headers := range map[string][]string{
		"another port":       {"Cookie", cookie, "Origin", "http://127.0.0.1:1", "Sec-Fetch-Site", "same-site"},
		"same site, no orig": {"Cookie", cookie, "Sec-Fetch-Site", "same-site"},
		"cross site":         {"Cookie", cookie, "Origin", "https://evil.example.com", "Sec-Fetch-Site", "cross-site"},
		"no signal":          {"Cookie", cookie},
		"forged cookie":      {"Cookie", sessionCookie + "=" + strings.Repeat("a", 64), "Sec-Fetch-Site", "same-origin"},
	} {
		if resp := a.raw("POST", "/v1/runs", headers...); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d", name, resp.StatusCode)
		}
	}
	if resp := a.raw("GET", "/v1/profiles", "Cookie", cookie, "Origin", own); resp.StatusCode != http.StatusOK {
		t.Errorf("an older browser with our Origin: %d", resp.StatusCode)
	}
}

// The token works as before; a page still cannot use it. A server without
// an app has no sign-in and no files.
func TestAppKeepsTheToken(t *testing.T) {
	a := appAPI(t)
	if resp := a.raw("GET", "/v1/profiles", "Authorization", "Bearer "+token); resp.StatusCode != http.StatusOK {
		t.Fatalf("token %d", resp.StatusCode)
	}
	if resp := a.raw("GET", "/v1/profiles", "Authorization", "Bearer "+token, "Origin", "http://127.0.0.1:1"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("token from a page %d", resp.StatusCode)
	}
	if code, out := a.do("POST", "/v1/signin-links", ""); code != http.StatusCreated || !strings.Contains(out, "/signin?code=") {
		t.Fatalf("link %d %s", code, out)
	}
	plain := newAPI(t, Config{Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}})
	if plain.srv.SignInPath() != "" {
		t.Fatal("a link without an app")
	}
	if resp := plain.raw("GET", "/signin?code=x"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("sign-in without an app: %d", resp.StatusCode)
	}
	if resp := plain.raw("GET", "/"); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("files without an app: %d", resp.StatusCode)
	}
}

func TestAppSecurityHardening(t *testing.T) {
	a := appAPI(t)
	if csp := a.raw("GET", "/").Header.Get("Content-Security-Policy"); !strings.Contains(csp, "media-src 'self' blob:") {
		t.Errorf("CSP missing media-src: %q", csp)
	}
	if dirResp := a.raw("GET", "/assets"); dirResp.StatusCode != http.StatusOK || !strings.Contains(body(dirResp), "the app") {
		t.Errorf("dir listing leaked or failed: %d %q", dirResp.StatusCode, body(dirResp))
	}
	link := a.srv.SignInPath()
	if head := a.raw("HEAD", link); head.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("HEAD /signin = %d, want 405", head.StatusCode)
	}
	if get := a.raw("GET", link); get.StatusCode != http.StatusSeeOther || get.Header.Get("Cache-Control") != "no-store" || get.Header.Get("Referrer-Policy") != "no-referrer" {
		t.Errorf("GET /signin = %d cc=%q ref=%q", get.StatusCode, get.Header.Get("Cache-Control"), get.Header.Get("Referrer-Policy"))
	}
}

func TestAppSignInLinkAndMountedGuard(t *testing.T) {
	a := appAPI(t)
	cookie := a.signedIn()
	if linkResp := a.raw("POST", "/v1/signin-links", "Cookie", cookie, "Sec-Fetch-Site", "same-origin"); linkResp.StatusCode != http.StatusForbidden {
		t.Errorf("browser cookie minted signin-link: %d", linkResp.StatusCode)
	}
	mounted := mustMountedAppServer(t)
	defer mounted.Close()
	resp, err := http.Get(mounted.URL + "/a2a")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unauth GET /a2a = %d, want 401", resp.StatusCode)
	}
}

func mustMountedAppServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, err := New(Config{Token: token, Profiles: []Profile{profile(t, "default", agent.Config{Provider: fake.New()})}, App: appFS})
	if err != nil {
		t.Fatal(err)
	}
	srv.Mount("POST /a2a", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	t.Cleanup(srv.Close)
	return httptest.NewServer(srv.Handler())
}
