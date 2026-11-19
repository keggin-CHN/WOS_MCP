package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/publicsuffix"
)

// newCookieJar creates a new cookie jar with public suffix list support.
func newCookieJar() (http.CookieJar, error) {
	return cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
}

// newUTLSTransport creates an http.Transport that spoofs Chrome's TLS fingerprint
// using the utls library. This is necessary to bypass the school's WAF which
// performs JA3 fingerprinting and blocks Go's standard TLS handshake.
// We clone Chrome's ClientHello but strip h2 from ALPN so the server
// falls back to HTTP/1.1 which our transport can handle.
func newUTLSTransport() *http.Transport {
	return &http.Transport{
		ForceAttemptHTTP2: false,
		DialTLSContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			conn, err := net.DialTimeout(network, addr, 15*time.Second)
			if err != nil {
				return nil, err
			}
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				host = addr
			}

			spec, err := utls.UTLSIdToSpec(utls.HelloChrome_Auto)
			if err != nil {
				conn.Close()
				return nil, fmt.Errorf("failed to get Chrome spec: %w", err)
			}

			for _, ext := range spec.Extensions {
				if alpn, ok := ext.(*utls.ALPNExtension); ok {
					alpn.AlpnProtocols = []string{"http/1.1"}
					break
				}
			}

			uconn := utls.UClient(conn, &utls.Config{ServerName: host}, utls.HelloCustom)
			if err := uconn.ApplyPreset(&spec); err != nil {
				conn.Close()
				return nil, fmt.Errorf("failed to apply preset: %w", err)
			}
			if err := uconn.HandshakeContext(ctx); err != nil {
				conn.Close()
				return nil, err
			}
			return uconn, nil
		},
	}
}

const (
	idpHost   = "https://idp-lib.njfu.edu.cn"
	wokHost   = "https://www.webofknowledge.com"
	wosHost   = "https://www.webofscience.com"
	cnkiFsso  = "https://fsso.cnki.net"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"

	// casRSAModulusHex is the 1024-bit public modulus published by the 2025
	// NJFU CAS login page (themes/sudy_njfu/js/login.js). New CAS deployments
	// no longer ship pwdDefaultEncryptSalt; instead the browser encrypts the
	// password with this textbook RSA key (exponent 0x10001) before submit.
	casRSAModulusHex = "008aed7e057fe8f14c73550b0e6467b023616ddc8fa91846d2613cdb7f7621e3cada4cd5d812d627af6b87727ade4e26d26208b7326815941492b2204c3167ab2d53df1e3a2c9153bdb7c8c2e968df97a5e7e01cc410f92c4c2c2fba529b3ee988ebc1fca99ff5119e036d732c368acf8beba01aa2fdafa45b21e4de4928d0d403"
	casRSAExponent   = 65537
	// casRSAChunkSize mirrors RSAUtils.encryptedString: 2 * biHighIndex(modulus).
	casRSAChunkSize = 126
)

var (
	wayflessURL = fmt.Sprintf("%s/?auth=ShibbolethIdPForm&entityID=%s&target=%s",
		wokHost, url.QueryEscape(idpHost+"/idp/shibboleth"), url.QueryEscape(wokHost+"/?DestApp=UA"))

	chars = "ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678"

	// ssoFormHosts are hosts on which we are willing to auto-submit the
	// Shibboleth/CAS forms encountered during login. Restricting submissions to
	// these hosts keeps us from blindly posting unrelated page forms.
	ssoFormHosts = map[string]bool{
		"idp-lib.njfu.edu.cn":       true,
		"uia.njfu.edu.cn":           true,
		"www.webofknowledge.com":    true,
		"webofknowledge.com":        true,
		"www.webofscience.com":      true,
		"webofscience.clarivate.cn": true,
		"fsso.cnki.net":             true,
	}

	// wosCookieHosts lists every host whose cookies are exported after login.
	wosCookieHosts = []string{
		"https://www.webofscience.com",
		"https://webofscience.com",
		"https://www.webofknowledge.com",
		"https://webofknowledge.com",
		"https://webofscience.clarivate.cn",
		"https://clarivate.cn",
		"https://www.clarivate.cn",
		"https://access.clarivate.com",
		"https://clarivate.com",
		"https://idp-lib.njfu.edu.cn",
		"https://uia.njfu.edu.cn",
	}
)

func rds(length int) string {
	b := make([]byte, length)
	rand.Read(b)
	for i := 0; i < length; i++ {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

func encryptPassword(password string, aesKey string) string {
	if aesKey == "" {
		return password
	}
	randomPrefix := rds(64)
	randomIV := rds(16)
	plaintext := append([]byte(randomPrefix), []byte(password)...)

	block, _ := aes.NewCipher([]byte(aesKey))
	padding := block.BlockSize() - len(plaintext)%block.BlockSize()
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	plaintext = append(plaintext, padtext...)

	ciphertext := make([]byte, len(plaintext))
	mode := cipher.NewCBCEncrypter(block, []byte(randomIV))
	mode.CryptBlocks(ciphertext, plaintext)

	return base64.StdEncoding.EncodeToString(ciphertext)
}

// rsaEncryptCasPassword reproduces RSAUtils.encryptedString from the CAS
// front-end: split the password into 126-byte blocks, pack each pair of bytes
// into a base-65536 digit, raise to e=65537 mod n, and emit fixed-width hex.
func rsaEncryptCasPassword(password string) (string, error) {
	n, ok := new(big.Int).SetString(casRSAModulusHex, 16)
	if !ok {
		return "", errors.New("invalid CAS RSA modulus")
	}
	e := big.NewInt(casRSAExponent)
	radix := big.NewInt(65536)

	data := []byte(password)
	var blocks []string
	for {
		chunk := make([]byte, casRSAChunkSize)
		copied := copy(chunk, data)
		data = data[copied:]

		m := new(big.Int)
		for i := 0; i < casRSAChunkSize; i += 2 {
			pair := big.NewInt(int64(chunk[i]) + int64(chunk[i+1])*256)
			exp := new(big.Int).Exp(radix, big.NewInt(int64(i/2)), nil)
			m.Add(m, pair.Mul(pair, exp))
		}
		c := new(big.Int).Exp(m, e, n)
		blocks = append(blocks, fmt.Sprintf("%0256x", c))

		if len(data) == 0 {
			break
		}
	}
	return strings.Join(blocks, " "), nil
}

// encryptCasPassword selects the password encoding used by the current CAS page:
// AES-CBC when the page still publishes pwdDefaultEncryptSalt, otherwise the
// hardcoded RSA public key used by the 2025 login page.
func encryptCasPassword(password, salt string) (string, error) {
	if salt != "" {
		return encryptPassword(password, salt), nil
	}
	return rsaEncryptCasPassword(password)
}

// locationSanitizingTransport intercepts HTTP responses and fixes malformed Location
// headers (e.g. from Clarivate SAML redirects) before Go's net/http url.Parse fails.
type locationSanitizingTransport struct {
	Transport http.RoundTripper
}

func (t *locationSanitizingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	tr := t.Transport
	if tr == nil {
		tr = http.DefaultTransport
	}
	resp, err := tr.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}

	if loc := resp.Header.Get("Location"); loc != "" {
		sanitized := sanitizeLocationHeader(loc)
		if sanitized != loc {
			log.Printf("[Transport] Sanitized malformed Location header:\n  Before: %s\n  After:  %s\n", loc, sanitized)
			resp.Header.Set("Location", sanitized)
		}
	}

	return resp, nil
}

// sanitizeLocationHeader repairs broken URLs in Location headers returned by servers.
// For example: "https://www.webofknowledge.comundefinednull&referrer=TARGET%3D..."
// is converted to "https://www.webofknowledge.com/?referrer=TARGET%3D...".
func sanitizeLocationHeader(loc string) string {
	if loc == "" {
		return loc
	}

	// 1. Strip JS undefined/null concatenation artifacts
	loc = strings.ReplaceAll(loc, "undefinednull", "")
	loc = strings.ReplaceAll(loc, "undefined", "")

	// 2. Fix missing / or ? between host and query parameters
	// e.g. "https://www.webofknowledge.com&referrer=..." -> "https://www.webofknowledge.com/?referrer=..."
	reMissingSlashOrQuery := regexp.MustCompile(`^(https?://[^/?#&]+)&(.*)$`)
	if m := reMissingSlashOrQuery.FindStringSubmatch(loc); len(m) > 2 {
		loc = m[1] + "/?" + m[2]
	}

	// 3. Fix "/&" without "?"
	reSlashAmp := regexp.MustCompile(`^(https?://[^/?#]+)/&(.*)$`)
	if m := reSlashAmp.FindStringSubmatch(loc); len(m) > 2 {
		loc = m[1] + "/?" + m[2]
	}

	return loc
}

// extractSID extracts a Web of Science SID from a given URL, error string, or body.
// Handles plain SID=..., single URL-encoded SID%3D..., and double URL-encoded SID%253D...
func extractSID(text string) string {
	if text == "" {
		return ""
	}

	// Direct regex: match SID=, SID%3D, SID%253D followed by alphanumeric session ID (>= 10 chars)
	re := regexp.MustCompile(`(?i)SID(?:%253D|%3D|=)([A-Za-z0-9]+)`)
	if m := re.FindStringSubmatch(text); len(m) > 1 && len(m[1]) >= 10 {
		return m[1]
	}

	// Try query unescaping once
	if unescaped, err := url.QueryUnescape(text); err == nil && unescaped != text {
		if m := re.FindStringSubmatch(unescaped); len(m) > 1 && len(m[1]) >= 10 {
			return m[1]
		}
		// Try query unescaping twice for double encoding
		if unescaped2, err2 := url.QueryUnescape(unescaped); err2 == nil && unescaped2 != unescaped {
			if m2 := re.FindStringSubmatch(unescaped2); len(m2) > 1 && len(m2[1]) >= 10 {
				return m2[1]
			}
		}
	}

	return ""
}

type WosLoginClient struct {
	client   *http.Client
	username string
	password string
}

func NewWosLoginClient(username, password string) *WosLoginClient {
	jar, _ := newCookieJar()
	client := &http.Client{
		Jar:       jar,
		Timeout:   30 * time.Second,
		Transport: &locationSanitizingTransport{Transport: newUTLSTransport()},
	}
	return &WosLoginClient{
		client:   client,
		username: username,
		password: password,
	}
}

func (c *WosLoginClient) doReq(req *http.Request) (*http.Response, string, *goquery.Document, error) {
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, "", nil, err
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewBuffer(body))
	doc, _ := goquery.NewDocumentFromReader(bytes.NewReader(body))
	return resp, string(body), doc, nil
}

func (c *WosLoginClient) postForm(action, referer string, data url.Values) (*http.Response, string, *goquery.Document, error) {
	req, err := http.NewRequest("POST", action, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, "", nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if referer != "" {
		req.Header.Set("Referer", referer)
	}
	return c.doReq(req)
}

// resolveAction turns a form action (absolute or relative) into an absolute URL
// based on the page that contained the form.
func resolveAction(pageURL, action string) string {
	if action == "" {
		return pageURL
	}
	if strings.HasPrefix(action, "http://") || strings.HasPrefix(action, "https://") {
		return action
	}
	base, err := url.Parse(pageURL)
	if err != nil {
		return action
	}
	rel, err := url.Parse(action)
	if err != nil {
		return action
	}
	return base.ResolveReference(rel).String()
}

func hostAllowedForSSO(host string) bool {
	return ssoFormHosts[strings.ToLower(host)]
}

// findCasLoginForm returns the credential form on the CAS login page. New CAS
// deployments use id="fm1"; older ones used id="casLoginForm". Both carry
// username/password inputs and a hidden execution/lt token.
func findCasLoginForm(doc *goquery.Document) *goquery.Selection {
	var found *goquery.Selection
	doc.Find("form").EachWithBreak(func(i int, s *goquery.Selection) bool {
		hasUser := s.Find("input[name='username']").Length() > 0
		hasPwd := s.Find("input[type='password']").Length() > 0 || s.Find("input[name='password']").Length() > 0
		if !hasUser || !hasPwd {
			return true
		}
		// Prefer the form that carries the flow token; the page contains helper
		// forms (scan/SMS) without one.
		if s.Find("input[name='execution']").Length() > 0 || s.Find("input[name='lt']").Length() > 0 {
			found = s
			return false
		}
		if found == nil {
			found = s
		}
		return true
	})
	return found
}

// isCasLoginPage reports whether the response is the CAS credential page.
func isCasLoginPage(pageURL string, doc *goquery.Document) bool {
	u, err := url.Parse(pageURL)
	if err != nil || !strings.EqualFold(u.Hostname(), "uia.njfu.edu.cn") {
		return false
	}
	return findCasLoginForm(doc) != nil
}

// selValue returns the first assigned value among the matched inputs.
func selValue(sel *goquery.Selection, attr string) string {
	v, _ := sel.Attr(attr)
	return v
}

// casFields collects the named inputs of a form, skipping submit controls and
// collapsing radio groups (preferring Shibboleth's "remember consent" option).
func casFields(form *goquery.Selection) url.Values {
	data := url.Values{}
	radios := map[string][]string{}
	form.Find("input").Each(func(i int, s *goquery.Selection) {
		name, ok := s.Attr("name")
		if !ok || name == "" {
			return
		}
		typ := strings.ToLower(selValue(s, "type"))
		val := selValue(s, "value")
		switch typ {
		case "submit", "button", "reset", "image", "file":
			return
		case "radio":
			radios[name] = append(radios[name], val)
		case "checkbox":
			data.Add(name, val)
		default:
			data.Set(name, val)
		}
	})
	for name, values := range radios {
		chosen := values[0]
		for _, v := range values {
			if v == "_shib_idp_rememberConsent" {
				chosen = v
				break
			}
		}
		data.Set(name, chosen)
	}
	return data
}

// formButton picks the submit button to emulate: Shibboleth's proceed button is
// preferred over alternatives such as "reject terms".
func formButton(form *goquery.Selection) string {
	var names []string
	form.Find("button").Each(func(i int, s *goquery.Selection) {
		if name, ok := s.Attr("name"); ok && name != "" {
			names = append(names, name)
		}
	})
	for _, n := range names {
		if n == "_eventId_proceed" {
			return n
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return ""
}

// isSSOAutoForm reports whether the form is part of the Shibboleth/CAS dance and
// therefore safe to submit automatically.
func isSSOAutoForm(form *goquery.Selection, pageURL string) bool {
	action := resolveAction(pageURL, selValue(form, "action"))
	u, err := url.Parse(action)
	if err != nil || !hostAllowedForSSO(u.Hostname()) {
		return false
	}
	for _, marker := range []string{"csrf_token", "SAMLRequest", "SAMLResponse", "execution", "lt"} {
		if form.Find(fmt.Sprintf("input[name='%s']", marker)).Length() > 0 {
			return true
		}
	}
	return false
}

// findSSOAutoForm returns the first auto-submittable SSO form on the page,
// preferring the one that carries a CSRF token.
func findSSOAutoForm(doc *goquery.Document, pageURL string) *goquery.Selection {
	var first *goquery.Selection
	doc.Find("form").EachWithBreak(func(i int, s *goquery.Selection) bool {
		if !isSSOAutoForm(s, pageURL) {
			return true
		}
		if s.Find("input[name='csrf_token']").Length() > 0 {
			first = s
			return false
		}
		if first == nil {
			first = s
		}
		return true
	})
	return first
}

// submitAutoForm posts the given SSO form, emulating the browser's automatic
// submission (used for SAML requests/responses and IdP interstitials).
func (c *WosLoginClient) submitAutoForm(form *goquery.Selection, pageURL string) (*http.Response, string, *goquery.Document, error) {
	action := resolveAction(pageURL, selValue(form, "action"))
	data := casFields(form)
	if button := formButton(form); button != "" {
		data.Set(button, "")
	}
	// The IdP's session-loading page reports whether localStorage is available.
	// We have no localStorage, so advertise exactly what the JS fallback sends.
	if data.Has("shib_idp_ls_supported") && data.Get("shib_idp_ls_supported") == "" {
		data.Set("shib_idp_ls_supported", "false")
	}
	return c.postForm(action, pageURL, data)
}

// casLogin submits credentials on the CAS page using whichever encoding the
// current deployment expects: AES with pwdDefaultEncryptSalt, or RSA with the
// page's published public key (2025 page).
func (c *WosLoginClient) casLogin(pageURL, body string, doc *goquery.Document) (*http.Response, string, *goquery.Document, error) {
	form := findCasLoginForm(doc)
	if form == nil {
		return nil, "", nil, errors.New("CAS login form not found")
	}
	action := resolveAction(pageURL, selValue(form, "action"))

	data := casFields(form)

	salt := ""
	for _, id := range []string{"pwdDefaultEncryptSalt", "dynamicPwdEncryptSalt"} {
		if sel := form.Find(fmt.Sprintf("input#%s", id)); sel.Length() > 0 {
			if v := strings.TrimSpace(selValue(sel, "value")); v != "" {
				salt = v
				break
			}
		}
	}
	if salt == "" {
		re := regexp.MustCompile(`pwdDefaultEncryptSalt\s*=\s*["']([^"']{8,64})["']`)
		if m := re.FindStringSubmatch(body); len(m) > 1 {
			salt = m[1]
		}
	}

	encPwd, err := encryptCasPassword(c.password, salt)
	if err != nil {
		return nil, "", nil, fmt.Errorf("encrypt CAS password: %w", err)
	}

	// Legacy deployments expect these helper fields; keep them when relevant.
	if data.Has("lt") {
		if !data.Has("dllt") {
			data.Set("dllt", "userNamePasswordLogin")
		}
		if !data.Has("rmShown") {
			data.Set("rmShown", "1")
		}
	}

	data.Set("username", strings.TrimSpace(c.username))
	data.Set("password", encPwd)

	log.Printf("CAS login: action=%s salt=%v fields=%v\n", action, salt != "", fieldNames(data))
	return c.postForm(action, pageURL, data)
}

func fieldNames(v url.Values) []string {
	out := make([]string, 0, len(v))
	for k := range v {
		out = append(out, k)
	}
	return out
}

// casLoginErrorMessage extracts the human-readable error from a CAS login page.
func casLoginErrorMessage(doc *goquery.Document) string {
	for _, sel := range []string{".form-error", "#msg", "span.auth_error", "#swiSpan1", ".auth_error"} {
		if msg := strings.TrimSpace(doc.Find(sel).First().Text()); msg != "" {
			return msg
		}
	}
	return ""
}

// completeSSO walks the Shibboleth/CAS conversation until no further SSO form
// needs submitting, logging into CAS with the stored credentials on the way.
// This covers the IdP's localStorage interstitial, the terms consent screens,
// the attribute-release consent screen and the final SAML response.
func (c *WosLoginClient) completeSSO(resp *http.Response, body string, doc *goquery.Document) (*http.Response, string, *goquery.Document, error) {
	if resp == nil || doc == nil {
		return resp, body, doc, errors.New("empty SSO response")
	}

	casAttempted := false
	for step := 0; step < 15; step++ {
		pageURL := resp.Request.URL.String()

		if isCasLoginPage(pageURL, doc) {
			if casAttempted {
				msg := casLoginErrorMessage(doc)
				if msg == "" {
					msg = "credentials rejected"
				}
				return resp, body, doc, fmt.Errorf("CAS login failed: %s", msg)
			}
			casAttempted = true
			var err error
			resp, body, doc, err = c.casLogin(pageURL, body, doc)
			if err != nil {
				return resp, body, doc, err
			}
			continue
		}

		form := findSSOAutoForm(doc, pageURL)
		if form == nil {
			return resp, body, doc, nil
		}

		log.Printf("SSO step %d: auto-submitting form on %s\n", step+1, pageURL)
		var err error
		resp, body, doc, err = c.submitAutoForm(form, pageURL)
		if err != nil {
			return resp, body, doc, err
		}
	}
	return resp, body, doc, errors.New("SSO did not terminate after 15 steps")
}

func (c *WosLoginClient) Login() (string, map[string]string, error) {
	log.Println("=== Starting WoS SSO Login ===")

	var sid string
	previousRedirect := c.client.CheckRedirect
	c.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 20 {
			return errors.New("too many redirects")
		}
		if s := extractSID(req.URL.String()); s != "" {
			sid = s
			log.Printf("Captured SID from redirect: %s\n", sid)
		}
		return nil
	}
	defer func() { c.client.CheckRedirect = previousRedirect }()

	req, err := http.NewRequest("GET", wayflessURL, nil)
	if err != nil {
		return "", nil, err
	}
	resp, body, doc, err := c.doReq(req)
	if err != nil {
		return "", nil, err
	}
	if !strings.Contains(body, "SAMLRequest") {
		return "", nil, fmt.Errorf("SAMLRequest form not found at %s", resp.Request.URL)
	}

	form := findSSOAutoForm(doc, resp.Request.URL.String())
	if form == nil {
		return "", nil, errors.New("SAMLRequest form not found")
	}
	resp, body, doc, err = c.submitAutoForm(form, resp.Request.URL.String())
	if err != nil {
		return "", nil, err
	}

	resp, body, doc, err = c.completeSSO(resp, body, doc)
	if err != nil {
		return "", nil, err
	}

	finalURL := resp.Request.URL.String()
	if isCasLoginPage(finalURL, doc) {
		msg := casLoginErrorMessage(doc)
		if msg == "" {
			msg = "unknown error"
		}
		return "", nil, fmt.Errorf("CAS login failed: %s", msg)
	}

	if sid == "" {
		sid = extractSID(finalURL)
	}
	if sid == "" {
		sid = extractSID(body)
	}
	if sid == "" {
		for _, domain := range []string{wokHost, wosHost, "https://webofscience.clarivate.cn"} {
			u, _ := url.Parse(domain)
			for _, ck := range c.client.Jar.Cookies(u) {
				if ck.Name == "SID" && ck.Value != "" {
					sid = ck.Value
				}
			}
		}
	}

	if sid == "" {
		return "", nil, fmt.Errorf("failed to extract SID from final URL: %s", finalURL)
	}

	log.Printf("=== Login Success! SID=%s (landed on %s) ===\n", sid, finalURL)

	cookies := make(map[string]string)
	for _, domain := range wosCookieHosts {
		u, _ := url.Parse(domain)
		for _, cookie := range c.client.Jar.Cookies(u) {
			cookies[cookie.Name] = cookie.Value
		}
	}
	return sid, cookies, nil
}
