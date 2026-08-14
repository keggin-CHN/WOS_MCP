package main

import (
	"bytes"
	"crypto/aes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// newClientUID returns a 32-hex clientUid matching what the CNKI JS frontend
// sends (real capture: "9137892a62e83ac6590676b562a9ffe7"). The old
// "slider-uuid-<ts>" prefix is a fingerprint that stands out across accounts.
func newClientUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("slider-uuid-%d", time.Now().UnixMilli()) // 兜底，不应发生
	}
	return hex.EncodeToString(b)
}

// extractReturnURL pulls the (URL-unescaped) returnUrl query param out of a
// verify/home redirect URL or a -403 captcha message. The web/check body needs
// the decoded form (capture [16]: "...FY4=" while the URL has "...FY4%3D").
func extractReturnURL(captchaSource string) string {
	reReturn := regexp.MustCompile(`returnUrl=([^&]+)`)
	m := reReturn.FindStringSubmatch(captchaSource)
	if len(m) > 1 {
		if dec, err := url.QueryUnescape(m[1]); err == nil {
			return dec
		}
		return m[1]
	}
	return ""
}

// CnkiResult represents a single CNKI search result.
type CnkiResult struct {
	Title    string
	Authors  string
	Source   string
	Date     string
	URL      string
	Abstract string
	DOI      string
	Filename string
	DBName   string
	DBCode   string
}

// CnkiClient handles CNKI session management and search.
type CnkiClient struct {
	session *http.Client
}

// NewCnkiClient creates a new CnkiClient with a default HTTP client.
func NewCnkiClient() *CnkiClient {
	jar, _ := newCookieJar()
	return &CnkiClient{
		session: &http.Client{
			Jar:       jar,
			Timeout:   30 * time.Second,
			Transport: newUTLSTransport(),
		},
	}
}

// ensureCnkiSession verifies the CNKI session or re-authenticates.
var cnkiSessionLock sync.Mutex
var captchaLock sync.Mutex
var lastCaptchaSolveTime time.Time

func (c *CnkiClient) ensureSession() error {
	cnkiSessionLock.Lock()
	defer cnkiSessionLock.Unlock()

	cfg := LoadConfig()
	u, _ := url.Parse("https://kns.cnki.net")

	// A usable CNKI session needs BOTH SID_kns_new and LID. LID is the login
	// token that lets us fetch article pages without clickWord captcha; without
	// it (e.g. config saved from a pre-fix build) every article page is blocked.
	hasSID, hasLID := false, false
	for _, cookie := range c.session.Jar.Cookies(u) {
		switch cookie.Name {
		case "SID_kns_new":
			hasSID = true
		case "LID":
			hasLID = true
		}
	}

	if !hasSID || !hasLID {
		if cookieMap, ok := cfg["cnki_cookies"].(map[string]interface{}); ok && len(cookieMap) > 0 {
			var cookies []*http.Cookie
			for k, v := range cookieMap {
				if vs, ok2 := v.(string); ok2 {
					cookies = append(cookies, &http.Cookie{Name: k, Value: vs})
				}
			}
			c.session.Jar.SetCookies(u, cookies)
			// re-check after injecting config
			hasSID, hasLID = false, false
			for _, cookie := range c.session.Jar.Cookies(u) {
				switch cookie.Name {
				case "SID_kns_new":
					hasSID = true
				case "LID":
					hasLID = true
				}
			}
		}
	}

	// Validate the session is actually usable (responds and not forced to login).
	if hasSID && hasLID {
		req, _ := http.NewRequest("GET", "https://kns.cnki.net/kns8s/AdvSearch", nil)
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.session.Do(req)
		if err == nil {
			resp.Body.Close()
			finalURL := resp.Request.URL.String()
			if resp.StatusCode == 200 && strings.Contains(finalURL, "kns.cnki.net") {
				return nil
			}
		}
	}

	username, password := GetCredentials(cfg)
	if username == "" || password == "" {
		return fmt.Errorf("CNKI credentials not configured")
	}

	err := c.login(username, password)
	if err != nil {
		return err
	}

	cookieMap := make(map[string]interface{})
	for _, ck := range c.session.Jar.Cookies(u) {
		cookieMap[ck.Name] = ck.Value
	}
	cfg["cnki_cookies"] = cookieMap
	SetCredentials(cfg, username, password)
	SaveConfig(cfg)

	return nil
}

// ---- CNKI burst-control throttling ----
//
// CNKI trips frequency control on bursts from one session (handoff doc
// §3.2/§4.1): after enough rapid requests, search + detail fetches start
// redirecting to verify/home even with a valid LID. We serialize search POSTs
// and detail-page GETs process-wide with a minimum gap so concurrent tool
// calls can't hammer the shared session into a flag.

var (
	cnkiSearchMu     sync.Mutex
	lastCnkiSearchAt time.Time
	cnkiDetailMu     sync.Mutex
	lastCnkiDetailAt time.Time
)

const (
	cnkiSearchGap = 1500 * time.Millisecond
	cnkiDetailGap = 2 * time.Second
)

// throttleCnkiSearch sleeps until ≥cnkiSearchGap since the last search POST,
// then records the time. Call right before issuing a kns8s/brief/grid POST.
func throttleCnkiSearch() {
	cnkiSearchMu.Lock()
	defer cnkiSearchMu.Unlock()
	if wait := cnkiSearchGap - time.Since(lastCnkiSearchAt); wait > 0 {
		time.Sleep(wait)
	}
	lastCnkiSearchAt = time.Now()
}

// serializedDetailFetch runs fn (a detail-page GET) while holding the global
// detail-fetch lock, so detail fetches never overlap and are ≥cnkiDetailGap
// apart. This is the global throttle of handoff doc §4.1.
func serializedDetailFetch(fn func()) {
	cnkiDetailMu.Lock()
	defer cnkiDetailMu.Unlock()
	if wait := cnkiDetailGap - time.Since(lastCnkiDetailAt); wait > 0 {
		fmt.Printf("CNKI throttle: sleeping %v before detail fetch\n", wait)
		time.Sleep(wait)
	}
	lastCnkiDetailAt = time.Now()
	fn()
}

// reloginLock serializes re-logins and lets us reuse a very-recent fresh login.
var (
	reloginLock   sync.Mutex
	lastReloginAt time.Time
)

// reloginFresh performs a full re-login on a brand-new session and returns the
// fresh client. Used when CNKI frequency-control flags the current session
// (verify/home redirects even with a valid LID): a new login issues a new LID,
// which resets the per-session burst state (handoff doc §4.1).
//
// Re-logins are serialized process-wide; if one succeeded within the last 10s
// we reuse its freshly-persisted cookies instead of hammering the SSO endpoint
// with another login (which could itself trip the CAS needCaptcha guard).
func reloginFresh() (*CnkiClient, error) {
	reloginLock.Lock()
	defer reloginLock.Unlock()

	cfg := LoadConfig()
	u, _ := url.Parse("https://kns.cnki.net")

	// Reuse a fresh login that another goroutine just performed.
	if time.Since(lastReloginAt) < 10*time.Second {
		if cookieMap, ok := cfg["cnki_cookies"].(map[string]interface{}); ok && len(cookieMap) > 0 {
			fresh := NewCnkiClient()
			var cookies []*http.Cookie
			for k, v := range cookieMap {
				if vs, ok2 := v.(string); ok2 {
					cookies = append(cookies, &http.Cookie{Name: k, Value: vs})
				}
			}
			fresh.session.Jar.SetCookies(u, cookies)
			fmt.Println("CNKI relogin: reusing cookies from recent fresh login")
			return fresh, nil
		}
	}

	username, password := GetCredentials(cfg)
	if username == "" || password == "" {
		return nil, fmt.Errorf("CNKI credentials not configured")
	}

	// Serialize against ensureSession's login too, so only one SSO login runs
	// at a time process-wide.
	cnkiSessionLock.Lock()
	defer cnkiSessionLock.Unlock()

	fresh := NewCnkiClient()
	if err := fresh.login(username, password); err != nil {
		return nil, err
	}

	// Defensive: a successful login must yield an LID cookie.
	hasLID := false
	for _, ck := range fresh.session.Jar.Cookies(u) {
		if ck.Name == "LID" {
			hasLID = true
			break
		}
	}
	if !hasLID {
		return nil, fmt.Errorf("relogin did not produce an LID cookie")
	}

	cookieMap := make(map[string]interface{})
	for _, ck := range fresh.session.Jar.Cookies(u) {
		cookieMap[ck.Name] = ck.Value
	}
	cfg["cnki_cookies"] = cookieMap
	SetCredentials(cfg, username, password)
	SaveConfig(cfg)
	lastReloginAt = time.Now()
	fmt.Println("CNKI relogin complete (fresh LID issued)")
	return fresh, nil
}

func (c *CnkiClient) login(username, password string) error {
	fmt.Println("Authenticating CNKI via SSO...")

	providerID := url.QueryEscape("https://fsso.cnki.net/shibboleth")
	target := url.QueryEscape("https://www.cnki.net")
	ssoURL := fmt.Sprintf("https://idp-lib.njfu.edu.cn/idp/profile/SAML2/Unsolicited/SSO?providerId=%s&target=%s", providerID, target)

	req, _ := http.NewRequest("GET", ssoURL, nil)
	req.Header.Set("User-Agent", userAgent)
	resp, err := c.session.Do(req)
	if err != nil {
		return fmt.Errorf("CNKI SSO trigger failed: %w", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	finalURL := resp.Request.URL.String()
	if !strings.Contains(finalURL, "uia.njfu.edu.cn") || !strings.Contains(finalURL, "login") {
		return fmt.Errorf("CNKI SSO failed, URL: %s", finalURL)
	}

	loginClient := NewWosLoginClient(username, password)

	loginClient.client.Jar = c.session.Jar

	doc, _ := goquery.NewDocumentFromReader(bytes.NewReader(body))
	form := doc.Find("#casLoginForm")
	if form.Length() == 0 {
		return fmt.Errorf("CAS login form not found")
	}
	var lt, execution, aesKey string
	lt, _ = form.Find("input[name='lt']").Attr("value")
	execution, _ = form.Find("input[name='execution']").Attr("value")
	loginAction, _ := form.Attr("action")
	if loginAction == "" {
		loginAction = finalURL
	} else if !strings.HasPrefix(loginAction, "http") {
		u, _ := url.Parse(finalURL)
		rel, _ := url.Parse(loginAction)
		loginAction = u.ResolveReference(rel).String()
	}

	aesKey, _ = form.Find("input#pwdDefaultEncryptSalt").Attr("value")
	if aesKey == "" {
		re := regexp.MustCompile(`pwdDefaultEncryptSalt\s*=\s*["']([^"']{8,32})["']`)
		m := re.FindStringSubmatch(string(body))
		if len(m) > 1 {
			aesKey = m[1]
		}
	}

	if lt == "" || execution == "" || aesKey == "" {
		return fmt.Errorf("failed to parse CAS login page for CNKI")
	}

	captchaURL := fmt.Sprintf("%s/authserver/needCaptcha.html?username=%s&pwdEncrypt2=pwdEncryptSalt&_=%d",
		casHost, url.QueryEscape(username), time.Now().UnixNano()/1e6)
	reqC, _ := http.NewRequest("GET", captchaURL, nil)
	reqC.Header.Set("User-Agent", userAgent)
	respC, err := loginClient.client.Do(reqC)
	if err == nil {
		bodyC, _ := io.ReadAll(respC.Body)
		respC.Body.Close()
		text := strings.TrimSpace(string(bodyC))
		if strings.HasPrefix(text, "true") || strings.Contains(text, "true::::") {
			return fmt.Errorf("captcha required for CNKI CAS login")
		}
	}

	encPwd := encryptPassword(password, aesKey)
	data := url.Values{}
	data.Set("username", strings.TrimSpace(username))
	data.Set("password", encPwd)
	data.Set("lt", strings.TrimSpace(lt))
	data.Set("dllt", "userNamePasswordLogin")
	data.Set("execution", strings.TrimSpace(execution))
	data.Set("_eventId", "submit")
	data.Set("rmShown", "1")

	reqL, _ := http.NewRequest("POST", loginAction, strings.NewReader(data.Encode()))
	reqL.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqL.Header.Set("User-Agent", userAgent)
	reqL.Header.Set("Origin", casHost)
	reqL.Header.Set("Referer", loginAction)
	reqL.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	reqL.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err = loginClient.client.Do(reqL)
	if err != nil {
		return fmt.Errorf("CAS login POST failed: %w", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	doc, _ = goquery.NewDocumentFromReader(bytes.NewReader(body))

	finalURL = resp.Request.URL.String()
	if strings.Contains(finalURL, "uia.njfu.edu.cn") && strings.Contains(finalURL, "/login") {
		msg := strings.TrimSpace(doc.Find("#msg").Text())
		if msg == "" {
			msg = strings.TrimSpace(doc.Find(".form-error").Text())
		}
		return fmt.Errorf("CNKI CAS login failed: %s", msg)
	}

	if strings.Contains(finalURL, "idp-lib.njfu.edu.cn") && (strings.Contains(string(body), "_shib_idp_consent") || strings.Contains(finalURL, "execution=e1s2")) {
		action, consentData, _ := parseAutoSubmitForm(doc)
		if !strings.HasPrefix(action, "http") {
			action = idpHost + action
		}
		consentData.Set("_shib_idp_consentOptions", "_shib_idp_rememberConsent")
		consentData.Set("_eventId_proceed", "Accept")
		reqCons, _ := http.NewRequest("POST", action, strings.NewReader(consentData.Encode()))
		reqCons.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqCons.Header.Set("User-Agent", userAgent)
		resp, err = loginClient.client.Do(reqCons)
		if err != nil {
			return fmt.Errorf("consent page failed: %w", err)
		}
		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
		doc, _ = goquery.NewDocumentFromReader(bytes.NewReader(body))
		finalURL = resp.Request.URL.String()
	}

	if strings.Contains(string(body), "SAMLResponse") {
		action, samlData, _ := parseAutoSubmitForm(doc)
		reqS, _ := http.NewRequest("POST", action, strings.NewReader(samlData.Encode()))
		reqS.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		reqS.Header.Set("User-Agent", userAgent)
		resp, err = loginClient.client.Do(reqS)
		if err != nil {
			return fmt.Errorf("SAML response submit failed: %w", err)
		}
		resp.Body.Close()
	}

	reqKNS, _ := http.NewRequest("GET", "https://kns.cnki.net/kns8s/AdvSearch", nil)
	reqKNS.Header.Set("User-Agent", userAgent)
	respKNS, err := c.session.Do(reqKNS)
	if err == nil {
		respKNS.Body.Close()
	}

	// Critical: the browser, after SAML returns, goes through CNKI's federation
	// callback https://fsso.cnki.net/secure/default.aspx which issues the LID +
	// Ecp_ClientId cookies. WITHOUT these, kns.cnki.net forces clickWord captcha
	// on every article page even when logged in. Visiting it once after login
	// makes the session able to fetch any article abstract without captcha.
	reqFSSO, _ := http.NewRequest("GET", "https://fsso.cnki.net/secure/default.aspx", nil)
	reqFSSO.Header.Set("User-Agent", userAgent)
	reqFSSO.Header.Set("Referer", "https://fsso.cnki.net/Shibboleth.sso/SAML2/POST")
	if respF, errF := c.session.Do(reqFSSO); errF == nil {
		io.Copy(io.Discard, respF.Body)
		respF.Body.Close()
		fmt.Println("CNKI fsso callback done (LID/Ecp_ClientId acquired)")
	}

	fmt.Println("CNKI login successful!")
	return nil
}

// solveCaptcha solves CNKI slider captcha using image processing.
// Pure Go port of the Python OpenCV-based solver (_solve_slider + _do_captcha_verify).
func solveCaptcha(session *http.Client, ident, captchaID string) bool {
	fmt.Printf("Triggering CAPTCHA bypass for ident=%s, captchaId=%s\n", ident, captchaID)

	referer := fmt.Sprintf("https://kns.cnki.net/verify/home?captchaType=blockPuzzle&ident=%s&captchaId=%s", ident, captchaID)

	dataGet := map[string]interface{}{
		"captchaType": "blockPuzzle",
		"clientUid":   fmt.Sprintf("slider-uuid-%d", time.Now().UnixMilli()),
		"ident":       ident,
		"captchaId":   captchaID,
		"ts":          time.Now().UnixMilli(),
	}

	jsonData, _ := json.Marshal(dataGet)
	req, _ := http.NewRequest("POST", "https://kns.cnki.net/verify-api/get", bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://kns.cnki.net")
	req.Header.Set("Referer", referer)

	resp, err := session.Do(req)
	if err != nil {
		fmt.Println("Captcha get failed:", err)
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	json.Unmarshal(body, &result)

	data, ok := result["data"].(map[string]interface{})
	if !ok {
		fmt.Println("Captcha data missing")
		return false
	}

	bgB64, _ := data["originalImageBase64"].(string)
	sliderB64, _ := data["jigsawImageBase64"].(string)
	token, _ := data["token"].(string)
	secretKey, _ := data["secretKey"].(string)
	if secretKey == "" && len(token) >= 16 {
		secretKey = token[:16]
	}

	if bgB64 == "" || sliderB64 == "" {
		fmt.Println("Captcha images missing")
		return false
	}

	bgBytes, err := base64.StdEncoding.DecodeString(bgB64)
	if err != nil {
		fmt.Println("Failed to decode bg image:", err)
		return false
	}
	sliderBytes, err := base64.StdEncoding.DecodeString(sliderB64)
	if err != nil {
		fmt.Println("Failed to decode slider image:", err)
		return false
	}

	bgImg, _, err := image.Decode(bytes.NewReader(bgBytes))
	if err != nil {
		fmt.Println("Failed to decode bg image format:", err)
		return false
	}
	sliderImg, _, err := image.Decode(bytes.NewReader(sliderBytes))
	if err != nil {
		fmt.Println("Failed to decode slider image format:", err)
		return false
	}

	offsetX := solveSliderOffset(bgImg, sliderImg)
	fmt.Printf("Computed slider offset: %d\n", offsetX)

	for _, adj := range []int{0, 1, -1, 2, -2, 3, -3} {
		x := offsetX + adj
		pointStr := fmt.Sprintf(`{"x":%d,"y":5.0}`, x)
		encrypted := encryptAESECB(pointStr, secretKey)

		checkData := map[string]interface{}{
			"captchaType": "blockPuzzle",
			"pointJson":   encrypted,
			"token":       token,
			"ident":       ident,
			"captchaId":   captchaID,
			"clientUid":   fmt.Sprintf("slider-uuid-%d", time.Now().UnixMilli()),
			"ts":          time.Now().UnixMilli(),
		}

		checkJSON, _ := json.Marshal(checkData)
		reqCheck, _ := http.NewRequest("POST", "https://kns.cnki.net/verify-api/web/check", bytes.NewBuffer(checkJSON))
		reqCheck.Header.Set("Content-Type", "application/json;charset=UTF-8")
		reqCheck.Header.Set("User-Agent", userAgent)
		reqCheck.Header.Set("Origin", "https://kns.cnki.net")
		reqCheck.Header.Set("Referer", referer)

		respCheck, err := session.Do(reqCheck)
		if err != nil {
			continue
		}
		bodyCheck, _ := io.ReadAll(respCheck.Body)
		respCheck.Body.Close()

		var checkResult map[string]interface{}
		json.Unmarshal(bodyCheck, &checkResult)
		if success, ok := checkResult["success"].(bool); ok && success {
			fmt.Printf("CAPTCHA bypass success with offset %d\n", x)
			return true
		}
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Println("CAPTCHA bypass failed after all attempts")
	return false
}

// encryptAESECB encrypts text with AES ECB mode (used for CNKI captcha).
func encryptAESECB(text, key string) string {
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		return ""
	}
	padLen := aes.BlockSize - len(text)%aes.BlockSize
	padded := []byte(text)
	for i := 0; i < padLen; i++ {
		padded = append(padded, byte(padLen))
	}
	encrypted := make([]byte, len(padded))
	for i := 0; i < len(padded); i += aes.BlockSize {
		block.Encrypt(encrypted[i:i+aes.BlockSize], padded[i:i+aes.BlockSize])
	}
	return base64.StdEncoding.EncodeToString(encrypted)
}

// pointListJSON renders a list of click points as the clickWord pointJson payload.
func pointListJSON(points [][2]int) string {
	parts := make([]string, 0, len(points))
	for _, p := range points {
		parts = append(parts, fmt.Sprintf(`{"x":%d,"y":%d}`, p[0], p[1]))
	}
	return "[" + strings.Join(parts, ",") + "]"
}

// runClickWordSolver shells out to the Python clickWord solver.
// Returns click points in word-list order.
func runClickWordSolver(imgBytes []byte, wordList []string) ([][2]int, error) {
	tmp, err := os.CreateTemp("", "clickword_*.jpg")
	if err != nil {
		return nil, err
	}
	imgPath := tmp.Name()
	defer os.Remove(imgPath)
	if _, err := tmp.Write(imgBytes); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()

	pyCmd := os.Getenv("CNKI_PYTHON")
	if pyCmd == "" {
		pyCmd = "python"
	}
	// locate the solver script: CNKI_SOLVER_DIR env, executable dir, then cwd
	solverPath := "clickword_solver.py"
	if d := os.Getenv("CNKI_SOLVER_DIR"); d != "" {
		solverPath = filepath.Join(d, "clickword_solver.py")
	} else if exe, err := os.Executable(); err == nil {
		exeDir := filepath.Dir(exe)
		if fi, err := os.Stat(filepath.Join(exeDir, "clickword_solver.py")); err == nil && !fi.IsDir() {
			solverPath = filepath.Join(exeDir, "clickword_solver.py")
		} else if cwd, err := os.Getwd(); err == nil {
			if fi, err := os.Stat(filepath.Join(cwd, "clickword_solver.py")); err == nil && !fi.IsDir() {
				solverPath = filepath.Join(cwd, "clickword_solver.py")
			}
		}
	}
	args := append([]string{solverPath, imgPath}, wordList...)
	cmd := exec.Command(pyCmd, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("clickword solver failed: %v\n%s", err, string(out))
	}

	// locate CLICKPOINTS_JSON= line
	var raw string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "CLICKPOINTS_JSON=") {
			raw = strings.TrimPrefix(line, "CLICKPOINTS_JSON=")
			break
		}
	}
	if raw == "" {
		return nil, fmt.Errorf("no CLICKPOINTS_JSON in solver output:\n%s", string(out))
	}
	var points [][2]int
	if err := json.Unmarshal([]byte(raw), &points); err != nil {
		return nil, fmt.Errorf("bad click points JSON %q: %v", raw, err)
	}
	if len(points) == 0 {
		return nil, fmt.Errorf("solver produced no points: %s", raw)
	}
	return points, nil
}

// solveClickWord solves the CNKI clickWord (点选文字) captcha end-to-end:
// fetch challenge -> OCR points via Python solver -> AES-ECB encrypt -> check.
// returnUrl is required by web/check (capture [16]); it is the decoded
// returnUrl query param from the verify/home redirect that triggered the
// challenge. Without it the server rejects with 6111.
func solveClickWord(session *http.Client, ident, captchaID, returnUrl string) bool {
	fmt.Printf("Triggering clickWord CAPTCHA solve for ident=%s, captchaId=%s\n", ident, captchaID)

	referer := fmt.Sprintf("https://kns.cnki.net/verify/home?captchaType=clickWord&ident=%s&captchaId=%s", ident, captchaID)
	if returnUrl != "" {
		referer += "&returnUrl=" + url.QueryEscape(returnUrl)
	}
	dataGet := map[string]interface{}{
		"captchaType": "clickWord",
		"clientUid":   newClientUID(),
		"ident":       ident,
		"captchaId":   captchaID,
		"ts":          time.Now().UnixMilli(),
	}
	jsonData, _ := json.Marshal(dataGet)
	req, _ := http.NewRequest("POST", "https://kns.cnki.net/verify-api/get", bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json;charset=UTF-8")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://kns.cnki.net")
	req.Header.Set("Referer", referer)

	resp, err := session.Do(req)
	if err != nil {
		fmt.Println("clickWord get failed:", err)
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		fmt.Printf("clickWord get bad JSON: %v\n%s\n", err, body)
		return false
	}
	data, ok := result["data"].(map[string]interface{})
	if !ok {
		fmt.Printf("clickWord data missing: %s\n", body)
		return false
	}

	bgB64, _ := data["originalImageBase64"].(string)
	token, _ := data["token"].(string)
	// The real secretKey is issued independently in the verify-api/get response.
	// token[:16] does NOT decrypt the real pointJson (verified against capture);
	// do not fall back to it — retry with a fresh challenge instead.
	secretKey, _ := data["secretKey"].(string)
	if secretKey == "" {
		fmt.Printf("clickWord secretKey missing, retry new challenge\n")
		return false
	}
	if bgB64 == "" {
		fmt.Printf("clickWord data incomplete (no image): %s\n", body)
		return false
	}

	imgBytes, err := base64.StdEncoding.DecodeString(bgB64)
	if err != nil {
		fmt.Println("clickWord decode image failed:", err)
		return false
	}

	var wordList []string
	if words, ok := data["wordList"].([]interface{}); ok {
		for _, w := range words {
			if s, ok := w.(string); ok {
				wordList = append(wordList, s)
			}
		}
	}
	if len(wordList) == 0 {
		fmt.Printf("clickWord wordList empty: %s\n", body)
		return false
	}
	fmt.Printf("clickWord wordList: %v\n", wordList)

	points, err := runClickWordSolver(imgBytes, wordList)
	if err != nil {
		fmt.Println(err)
		return false
	}
	fmt.Printf("clickWord points: %v\n", points)

	encrypted := encryptAESECB(pointListJSON(points), secretKey)
	// Body mirrors the real manual submission (capture [16]) field-for-field:
	// captchaType, pointJson, token, ident, returnUrl, captchaId. The browser
	// sends no clientUid/ts here, so we don't either.
	checkData := map[string]interface{}{
		"captchaType": "clickWord",
		"pointJson":   encrypted,
		"token":       token,
		"ident":       ident,
		"returnUrl":   returnUrl,
		"captchaId":   captchaID,
	}
	checkJSON, _ := json.Marshal(checkData)
	reqCheck, _ := http.NewRequest("POST", "https://kns.cnki.net/verify-api/web/check", bytes.NewBuffer(checkJSON))
	reqCheck.Header.Set("Content-Type", "application/json;charset=UTF-8")
	reqCheck.Header.Set("User-Agent", userAgent)
	reqCheck.Header.Set("Origin", "https://kns.cnki.net")
	reqCheck.Header.Set("Referer", referer)

	respCheck, err := session.Do(reqCheck)
	if err != nil {
		fmt.Println("clickWord check failed:", err)
		return false
	}
	defer respCheck.Body.Close()
	bodyCheck, _ := io.ReadAll(respCheck.Body)
	var checkResult map[string]interface{}
	json.Unmarshal(bodyCheck, &checkResult)
	if success, ok := checkResult["success"].(bool); ok && success {
		fmt.Printf("clickWord CAPTCHA bypass success!\n")
		return true
	}
	fmt.Printf("clickWord check rejected: %s\n", bodyCheck)
	return false
}

// Search performs a CNKI literature search.
func (c *CnkiClient) Search(query, searchType string, limit int) ([]CnkiResult, error) {
	if err := c.ensureSession(); err != nil {
		return nil, err
	}

	// Serialize search POSTs process-wide (handoff doc §4.1 burst control).
	throttleCnkiSearch()

	stMap := map[string]string{
		"主题": "SU",
		"篇名": "TI",
		"全文": "KY",
		"作者": "AU",
		"机构": "AF",
	}
	stCode, ok := stMap[searchType]
	if !ok {
		stCode = searchType
	}

	queryJSON := map[string]interface{}{
		"Platform": "",
		"Resource": "CROSSDB",
		"Classid":  "WD0FTY92",
		"Products": "",
		"QNode": map[string]interface{}{
			"QGroup": []map[string]interface{}{
				{
					"Key":   "Subject",
					"Title": "",
					"Logic": 0,
					"Items": []map[string]interface{}{
						{
							"Field":    stCode,
							"Value":    query,
							"Operator": "TOPRANK",
							"Logic":    0,
							"Title":    "检索项",
						},
					},
					"ChildItems": []interface{}{},
				},
			},
		},
		"ExScope":    1,
		"SearchType": 2,
		"Rlang":      "CHINESE",
		"KuaKuCode":  "YSTT4HG0,LSTPFY1C,EMRPGLPA,JUP3MUPD,MPMFIG1A,WQ0UVIAA,BLZOG7CK,PWFIRAGL,NN3FJMUV,NLBO1Z6R",
		"Expands":    map[string]interface{}{},
		"View":       "changeDBCh",
		"SearchFrom": 1,
	}
	qjBytes, _ := json.Marshal(queryJSON)
	encodedQJ := url.QueryEscape(string(qjBytes))
	payload := fmt.Sprintf("boolSearch=true&QueryJson=%s&pageNum=1&pageSize=%d&sortField=&sortType=&dstyle=abstractmode&productStr=&aside=&searchFrom=%s&subject=&language=&uniplatform=&CurPage=1",
		encodedQJ, limit, url.QueryEscape("资源范围：总库"))

	var body []byte
	var err error
	var resp *http.Response

	var currentCaptchaID string
	for attempt := 0; attempt < 3; attempt++ {
		targetURL := "https://kns.cnki.net/kns8s/brief/grid"
		if currentCaptchaID != "" {
			targetURL += "?captchaId=" + currentCaptchaID
		}
		req, errReq := http.NewRequest("POST", targetURL, strings.NewReader(payload))
		if errReq != nil {
			return nil, errReq
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=UTF-8")
		req.Header.Set("User-Agent", userAgent)
		req.Header.Set("Origin", "https://kns.cnki.net")
		req.Header.Set("Referer", "https://kns.cnki.net/kns8s/defaultresult/index?kw="+url.QueryEscape(query))

		resp, err = c.session.Do(req)
		if err != nil {
			return nil, err
		}

		body, _ = io.ReadAll(resp.Body)
		resp.Body.Close()

		finalURL := resp.Request.URL.String()

		// Detect captcha: either HTML redirect to verify/home, or JSON -403 with captcha URL in message
		captchaSource := ""
		if strings.Contains(finalURL, "verify/home") {
			captchaSource = finalURL
		} else if resp.StatusCode == 403 {
			// Try to parse JSON body for captcha URL
			var jsonResp struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			}
			if jsonErr := json.Unmarshal(body, &jsonResp); jsonErr == nil && jsonResp.Code == -403 {
				captchaSource = jsonResp.Message
			} else {
				captchaSource = string(body)
			}
		}

		if captchaSource != "" && strings.Contains(captchaSource, "captchaType=blockPuzzle") {
			captchaLock.Lock()
			needsCaptcha := true
			if time.Since(lastCaptchaSolveTime) < 5*time.Second {
				needsCaptcha = false
			}

			if needsCaptcha {
				reIdent := regexp.MustCompile(`ident=([a-zA-Z0-9]+)`)
				reCaptchaID := regexp.MustCompile(`captchaId=([a-zA-Z0-9\-]+)`)

				mIdent := reIdent.FindStringSubmatch(captchaSource)
				mCaptchaID := reCaptchaID.FindStringSubmatch(captchaSource)
				if len(mIdent) > 1 && len(mCaptchaID) > 1 {
					currentCaptchaID = mCaptchaID[1]
					solved := false
					for c_attempt := 0; c_attempt < 3; c_attempt++ {
						if solveCaptcha(c.session, mIdent[1], currentCaptchaID) {
							solved = true
							break
						}
						fmt.Printf("Captcha attempt %d failed, retrying with new image...\n", c_attempt+1)
						time.Sleep(500 * time.Millisecond)
					}
					if solved {
						lastCaptchaSolveTime = time.Now()

						cfg := LoadConfig()
						u, _ := url.Parse("https://kns.cnki.net")
						cookieMap := make(map[string]interface{})
						for _, ck := range c.session.Jar.Cookies(u) {
							cookieMap[ck.Name] = ck.Value
						}
						cfg["cnki_cookies"] = cookieMap
						SaveConfig(cfg)
					} else {
						fmt.Printf("CAPTCHA bypass failed after all attempts\n")
					}
				}
			}
			captchaLock.Unlock()

			time.Sleep(3 * time.Second)
			continue
		} else if captchaSource != "" && strings.Contains(captchaSource, "captchaType=clickWord") {
			captchaLock.Lock()
			needsCaptcha := true
			if time.Since(lastCaptchaSolveTime) < 5*time.Second {
				needsCaptcha = false
			}

			if needsCaptcha {
				reIdent := regexp.MustCompile(`ident=([a-zA-Z0-9]+)`)
				reCaptchaID := regexp.MustCompile(`captchaId=([a-zA-Z0-9\-]+)`)
				mIdent := reIdent.FindStringSubmatch(captchaSource)
				mCaptchaID := reCaptchaID.FindStringSubmatch(captchaSource)
				if len(mIdent) > 1 && len(mCaptchaID) > 1 {
					currentCaptchaID = mCaptchaID[1]
					returnUrl := extractReturnURL(captchaSource)
					solved := false
					for c_attempt := 0; c_attempt < 3; c_attempt++ {
						if solveClickWord(c.session, mIdent[1], currentCaptchaID, returnUrl) {
							solved = true
							break
						}
						fmt.Printf("clickWord attempt %d failed, retrying with new image...\n", c_attempt+1)
						time.Sleep(800 * time.Millisecond)
					}
					if solved {
						lastCaptchaSolveTime = time.Now()
						cfg := LoadConfig()
						u, _ := url.Parse("https://kns.cnki.net")
						cookieMap := make(map[string]interface{})
						for _, ck := range c.session.Jar.Cookies(u) {
							cookieMap[ck.Name] = ck.Value
						}
						cfg["cnki_cookies"] = cookieMap
						SaveConfig(cfg)
					} else {
						fmt.Printf("clickWord CAPTCHA bypass failed after all attempts\n")
					}
				}
			}
			captchaLock.Unlock()
			time.Sleep(2 * time.Second)
			continue
		}

		if resp.StatusCode == 200 {
			break
		}
	}

	if resp != nil && resp.StatusCode == 403 {
		return nil, fmt.Errorf("Retry request failed with status 403")
	}

	if len(body) == 0 {
		return nil, fmt.Errorf("empty response body")
	}

	doc, _ := goquery.NewDocumentFromReader(bytes.NewReader(body))
	rows := doc.Find("table.result-table-list tbody tr")

	var results []CnkiResult
	rows.Each(func(i int, s *goquery.Selection) {
		if i >= limit {
			return
		}
		if i == 0 {
			html, _ := s.Html()
			fmt.Printf("DEBUG ROW 0 HTML: %s\n", html)
		}
		titleElem := s.Find("td.name a")
		if titleElem.Length() == 0 {
			return
		}
		title := strings.TrimSpace(titleElem.Text())
		href, _ := titleElem.Attr("href")

		var authors []string
		s.Find("td.author a").Each(func(_ int, a *goquery.Selection) {
			authors = append(authors, strings.TrimSpace(a.Text()))
		})

		source := ""
		if srcElem := s.Find("td.source a"); srcElem.Length() > 0 {
			source = strings.TrimSpace(srcElem.Text())
		}

		date := ""
		if dateElem := s.Find("td.date"); dateElem.Length() > 0 {
			date = strings.TrimSpace(dateElem.Text())
		}

		// Extract article metadata from data-* attributes on the row
		filename, _ := s.Attr("data-filename")
		dbname, _ := s.Attr("data-dbname")
		dbcode, _ := s.Attr("data-dbcode")

		// Try to parse filename from href if not in data attr
		if filename == "" {
			reFilename := regexp.MustCompile(`[Ff]ile[Nn]ame=([A-Z0-9]+)`)
			if m := reFilename.FindStringSubmatch(href); len(m) > 1 {
				filename = m[1]
			}
		}

		// Extract DOI from the result row
		doi := ""
		if doiElem := s.Find(".doi a, [data-doi]"); doiElem.Length() > 0 {
			doi = strings.TrimSpace(doiElem.Text())
			if doi == "" {
				doi, _ = doiElem.Attr("data-doi")
			}
		}

		// Extract abstract snippet
		abstract := ""
		if absElem := s.Find(".abstract, .desc, p.abstract, .brief"); absElem.Length() > 0 {
			abstract = strings.TrimSpace(absElem.Text())
		}

		resultURL := href
		if strings.HasPrefix(resultURL, "/") {
			resultURL = "https://kns.cnki.net" + resultURL
		} else if !strings.HasPrefix(resultURL, "http") {
			resultURL = "https://kns.cnki.net/kns8s/" + resultURL
		}

		results = append(results, CnkiResult{
			Title:    title,
			Authors:  strings.Join(authors, "; "),
			Source:   source,
			Date:     date,
			URL:      resultURL,
			Abstract: abstract,
			DOI:      doi,
			Filename: filename,
			DBName:   dbname,
			DBCode:   dbcode,
		})
	})

	return results, nil
}

// GetArticleDetail fetches detailed info for a single article by title search.
// This avoids the clickWord-protected abstract page entirely.
func (c *CnkiClient) GetArticleDetail(title string) (*CnkiResult, error) {
	// Search by title (exact phrase match via TI field)
	results, err := c.Search(title, "TI", 1)
	if err != nil {
		// Fallback to topic search
		results, err = c.Search(title, "SU", 1)
		if err != nil {
			return nil, err
		}
	}
	if len(results) == 0 {
		return nil, fmt.Errorf("article not found")
	}
	return &results[0], nil
}
