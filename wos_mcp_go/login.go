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
	casHost   = "https://uia.njfu.edu.cn"
	idpHost   = "https://idp-lib.njfu.edu.cn"
	wokHost   = "https://www.webofknowledge.com"
	wosHost   = "https://www.webofscience.com"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
)

var (
	wayflessURL = fmt.Sprintf("%s/?auth=ShibbolethIdPForm&entityID=%s&target=%s",
		wokHost, url.QueryEscape(idpHost+"/idp/shibboleth"), url.QueryEscape(wokHost+"/?DestApp=UA"))

	chars = "ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678"
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

func parseAutoSubmitForm(doc *goquery.Document) (string, url.Values, error) {
	form := doc.Find("form").First()
	if form.Length() == 0 {
		return "", nil, errors.New("form not found")
	}
	action, _ := form.Attr("action")
	data := url.Values{}
	form.Find("input").Each(func(i int, s *goquery.Selection) {
		name, ok := s.Attr("name")
		if !ok || name == "" {
			return
		}
		val, _ := s.Attr("value")
		typ, _ := s.Attr("type")
		typ = strings.ToLower(typ)
		if typ != "submit" && typ != "button" && typ != "reset" {
			data.Add(name, val)
		}
	})
	return action, data, nil
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
	resp.Body = io.NopCloser(bytes.NewBuffer(body))
	doc, _ := goquery.NewDocumentFromReader(bytes.NewReader(body))
	return resp, string(body), doc, nil
}

func (c *WosLoginClient) Login() (string, map[string]string, error) {
	log.Println("=== Starting WoS SSO Login ===")

	req, _ := http.NewRequest("GET", wayflessURL, nil)
	resp, body, doc, err := c.doReq(req)
	if err != nil {
		return "", nil, err
	}

	if !strings.Contains(body, "SAMLRequest") {
		return "", nil, errors.New("SAMLRequest form not found")
	}
	action, formData, err := parseAutoSubmitForm(doc)
	if err != nil {
		return "", nil, err
	}
	req, _ = http.NewRequest("POST", action, strings.NewReader(formData.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, body, doc, err = c.doReq(req)
	if err != nil {
		return "", nil, err
	}

	finalURL := resp.Request.URL.String()
	if !strings.Contains(finalURL, "uia.njfu.edu.cn") {
		return "", nil, fmt.Errorf("SSO trigger failed, URL: %s", finalURL)
	}

	form := doc.Find("#casLoginForm")
	if form.Length() == 0 {
		return "", nil, errors.New("casLoginForm not found")
	}
	loginAction, _ := form.Attr("action")
	if loginAction == "" {
		loginAction = finalURL
	} else if !strings.HasPrefix(loginAction, "http") {
		u, _ := url.Parse(finalURL)
		rel, _ := url.Parse(loginAction)
		loginAction = u.ResolveReference(rel).String()
	}

	var lt, execution, aesKey string
	lt, _ = form.Find("input[name='lt']").Attr("value")
	execution, _ = form.Find("input[name='execution']").Attr("value")
	aesKey, _ = form.Find("input#dynamicPwdEncryptSalt").Attr("value")
	if aesKey == "" {
		aesKey, _ = form.Find("input#pwdDefaultEncryptSalt").Attr("value")
	}
	if aesKey == "" {
		re := regexp.MustCompile(`pwdDefaultEncryptSalt\s*=\s*["']([^"']{8,32})["']`)
		m := re.FindStringSubmatch(body)
		if len(m) > 1 {
			aesKey = m[1]
		}
	}
	log.Printf("Parsed: lt=%s..., execution=%s, aesKey=%s\n", lt[:min(30, len(lt))], execution, aesKey)

	if lt == "" || execution == "" || aesKey == "" {
		return "", nil, errors.New("failed to parse CAS login page inputs")
	}

	captchaURL := fmt.Sprintf("%s/authserver/needCaptcha.html?username=%s&pwdEncrypt2=pwdEncryptSalt&_=%d",
		casHost, url.QueryEscape(c.username), time.Now().UnixNano()/1e6)
	reqC, _ := http.NewRequest("GET", captchaURL, nil)
	respC, bodyC, _, _ := c.doReq(reqC)
	respC.Body.Close()
	text := strings.TrimSpace(bodyC)
	log.Printf("needCaptcha response: [%s]\n", text)

	if strings.Contains(text, "::::") {
		parts := strings.SplitN(text, "::::", 2)
		if strings.TrimSpace(strings.ToLower(parts[0])) == "true" {
			return "", nil, errors.New("captcha required for CAS login, not supported yet")
		}
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			newSalt := strings.TrimSpace(parts[1])
			log.Printf("Got new encryption salt from needCaptcha: %s (replacing old: %s)\n", newSalt, aesKey)
			aesKey = newSalt
		}
	} else if strings.TrimSpace(strings.ToLower(text)) == "true" {
		return "", nil, errors.New("captcha required for CAS login, not supported yet")
	}

	encPwd := encryptPassword(c.password, aesKey)

	payloadStr := fmt.Sprintf("username=%s&password=%s&lt=%s&dllt=userNamePasswordLogin&execution=%s&_eventId=submit&rmShown=1",
		url.QueryEscape(strings.TrimSpace(c.username)),
		url.QueryEscape(encPwd),
		url.QueryEscape(strings.TrimSpace(lt)),
		url.QueryEscape(strings.TrimSpace(execution)))

	reqL, _ := http.NewRequest("POST", loginAction, strings.NewReader(payloadStr))
	reqL.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqL.Header.Set("Origin", casHost)
	reqL.Header.Set("Referer", loginAction)
	reqL.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	reqL.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6")

	log.Printf("POSTing login to %s\n", loginAction)
	resp, body, doc, err = c.doReq(reqL)
	if err != nil {
		return "", nil, err
	}

	finalURL = resp.Request.URL.String()
	log.Printf("After CAS login, finalURL=%s\n", finalURL)
	if strings.Contains(finalURL, "uia.njfu.edu.cn") && strings.Contains(finalURL, "/login") {
		msg := strings.TrimSpace(doc.Find("#msg").Text())
		if msg == "" {
			msg = strings.TrimSpace(doc.Find(".form-error").Text())
		}
		if msg == "" {
			msg = strings.TrimSpace(doc.Find("span.auth_error").Text())
		}
		return "", nil, fmt.Errorf("login failed: %s", msg)
	}

	if strings.Contains(finalURL, "idp-lib.njfu.edu.cn") && (strings.Contains(body, "_shib_idp_consent") || strings.Contains(finalURL, "execution=e1s2")) {
		log.Println("Handling IDP Consent Page")
		action, consentData, _ := parseAutoSubmitForm(doc)
		if !strings.HasPrefix(action, "http") {
			action = idpHost + action
		}
		consentData.Set("_shib_idp_consentOptions", "_shib_idp_rememberConsent")
		consentData.Set("_eventId_proceed", "Accept")
		reqCons, _ := http.NewRequest("POST", action, strings.NewReader(consentData.Encode()))
		reqCons.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, body, doc, err = c.doReq(reqCons)
		if err != nil {
			return "", nil, err
		}
		finalURL = resp.Request.URL.String()
	}

	// Step 8-9: POST SAMLResponse
	var sid string
	if strings.Contains(body, "SAMLResponse") {
		log.Println("Detected SAMLResponse, auto-submitting...")
		action, samlData, _ := parseAutoSubmitForm(doc)
		if !strings.HasPrefix(action, "http") {
			action = wokHost + "/" + strings.TrimLeft(action, "/")
		}
		reqS, _ := http.NewRequest("POST", action, strings.NewReader(samlData.Encode()))
		reqS.Header.Set("Content-Type", "application/x-www-form-urlencoded")

		// In Go, http.Client automatically follows redirects. The SID might be in the URL of one of the redirects.
		var errStopRedirect = errors.New("stop: SID captured")
		c.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			log.Printf("Redirect to: %s\n", req.URL.String())
			if s := extractSID(req.URL.String()); s != "" {
				sid = s
				log.Printf("Captured SID from redirect: %s\n", sid)
			}

			if sid != "" {
				return errStopRedirect
			}
			if len(via) >= 15 {
				return errors.New("too many redirects")
			}
			return nil
		}
		resp, body, _, err = c.doReq(reqS)
		c.client.CheckRedirect = nil
		if err != nil {
			if sid == "" {
				sid = extractSID(err.Error())
				if sid != "" {
					log.Printf("Captured SID from redirect error message: %s\n", sid)
				}
			}
			if sid != "" {
				log.Printf("Redirect finished with captured SID: %s (status info: %v)\n", sid, err)
			} else {
				return "", nil, err
			}
		}
		if resp != nil {
			finalURL = resp.Request.URL.String()
		}
	}

	if sid == "" {
		sid = extractSID(finalURL)
	}
	if sid == "" {
		sid = extractSID(body)
	}

	if sid == "" {
		return "", nil, fmt.Errorf("failed to extract SID from final URL: %s", finalURL)
	}

	log.Printf("=== Login Success! SID=%s ===\n", sid)

	cookies := make(map[string]string)
	for _, domain := range []string{
		"https://www.webofscience.com",
		"https://webofscience.com",
		"https://www.webofknowledge.com",
		"https://webofknowledge.com",
		"https://access.clarivate.com",
		"https://clarivate.com",
		"https://idp-lib.njfu.edu.cn",
		"https://uia.njfu.edu.cn",
	} {
		u, _ := url.Parse(domain)
		for _, cookie := range c.client.Jar.Cookies(u) {
			cookies[cookie.Name] = cookie.Value
		}
	}
	return sid, cookies, nil
}
