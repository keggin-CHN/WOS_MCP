package main

import (
	"bytes"
	"crypto/aes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/PuerkitoBio/goquery"
)

// CnkiResult represents a single CNKI search result.
type CnkiResult struct {
	Title	string
	Authors	string
	Source	string
	Date	string
	URL	string
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
			Jar:		jar,
			Timeout:	30 * time.Second,
			Transport:	newUTLSTransport(),
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

	hasSession := false
	for _, cookie := range c.session.Jar.Cookies(u) {
		if cookie.Name == "SID_kns_new" {
			hasSession = true
			break
		}
	}

	if !hasSession {

		if cookieMap, ok := cfg["cnki_cookies"].(map[string]interface{}); ok && len(cookieMap) > 0 {
			var cookies []*http.Cookie
			for k, v := range cookieMap {
				if vs, ok2 := v.(string); ok2 {
					cookies = append(cookies, &http.Cookie{Name: k, Value: vs})
				}
			}
			c.session.Jar.SetCookies(u, cookies)
		}
	}

	if len(c.session.Jar.Cookies(u)) > 0 {

		req, _ := http.NewRequest("GET", "https://kns.cnki.net/kns8s/AdvSearch", nil)
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.session.Do(req)
		if err == nil {
			defer resp.Body.Close()
			finalURL := resp.Request.URL.String()
			if resp.StatusCode == 200 {

				if strings.Contains(finalURL, "kns.cnki.net") {
					return nil
				}
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

	fmt.Println("CNKI login successful!")
	return nil
}

// solveCaptcha solves CNKI slider captcha using image processing.
// Pure Go port of the Python OpenCV-based solver (_solve_slider + _do_captcha_verify).
func solveCaptcha(session *http.Client, ident, captchaID string) bool {
	fmt.Printf("Triggering CAPTCHA bypass for ident=%s, captchaId=%s\n", ident, captchaID)

	referer := fmt.Sprintf("https://kns.cnki.net/verify/home?captchaType=blockPuzzle&ident=%s&captchaId=%s", ident, captchaID)

	dataGet := map[string]interface{}{
		"captchaType":	"blockPuzzle",
		"clientUid":	fmt.Sprintf("slider-uuid-%d", time.Now().UnixMilli()),
		"ident":	ident,
		"captchaId":	captchaID,
		"ts":		time.Now().UnixMilli(),
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
			"captchaType":	"blockPuzzle",
			"pointJson":	encrypted,
			"token":	token,
			"ident":	ident,
			"captchaId":	captchaID,
			"clientUid":	fmt.Sprintf("slider-uuid-%d", time.Now().UnixMilli()),
			"ts":		time.Now().UnixMilli(),
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

// Search performs a CNKI literature search.
func (c *CnkiClient) Search(query, searchType string, limit int) ([]CnkiResult, error) {
	if err := c.ensureSession(); err != nil {
		return nil, err
	}

	stMap := map[string]string{
		"主题":	"SU",
		"篇名":	"TI",
		"全文":	"KY",
		"作者":	"AU",
		"机构":	"AF",
	}
	stCode, ok := stMap[searchType]
	if !ok {
		stCode = searchType
	}

	queryJSON := map[string]interface{}{
		"Platform":	"",
		"Resource":	"CROSSDB",
		"Classid":	"WD0FTY92",
		"Products":	"",
		"QNode": map[string]interface{}{
			"QGroup": []map[string]interface{}{
				{
					"Key":		"Subject",
					"Title":	"",
					"Logic":	0,
					"Items": []map[string]interface{}{
						{
							"Field":	stCode,
							"Value":	query,
							"Operator":	"TOPRANK",
							"Logic":	0,
							"Title":	"检索项",
						},
					},
					"ChildItems":	[]interface{}{},
				},
			},
		},
		"ExScope":	1,
		"SearchType":	2,
		"Rlang":	"CHINESE",
		"KuaKuCode":	"YSTT4HG0,LSTPFY1C,EMRPGLPA,JUP3MUPD,MPMFIG1A,WQ0UVIAA,BLZOG7CK,PWFIRAGL,NN3FJMUV,NLBO1Z6R",
		"Expands":	map[string]interface{}{},
		"View":		"changeDBCh",
		"SearchFrom":	1,
	}
	qjBytes, _ := json.Marshal(queryJSON)
	encodedQJ := url.QueryEscape(string(qjBytes))
	payload := fmt.Sprintf("boolSearch=true&QueryJson=%s&pageNum=1&pageSize=%d&sortField=&sortType=&dstyle=listmode&productStr=&aside=&searchFrom=%s&subject=&language=&uniplatform=&CurPage=1",
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
		if strings.Contains(finalURL, "verify/home") || resp.StatusCode == 403 {
			captchaLock.Lock()
			needsCaptcha := true
			if time.Since(lastCaptchaSolveTime) < 5*time.Second {
				needsCaptcha = false
			}

			if needsCaptcha {

				reIdent := regexp.MustCompile(`ident=([a-zA-Z0-9]+)`)
				reCaptchaID := regexp.MustCompile(`captchaId=([a-zA-Z0-9\-]+)`)

				captchaPageBody := body

				mIdent := reIdent.FindStringSubmatch(string(captchaPageBody))
				mCaptchaID := reCaptchaID.FindStringSubmatch(string(captchaPageBody))
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

		resultURL := href
		if strings.HasPrefix(resultURL, "/") {
			resultURL = "https://kns.cnki.net" + resultURL
		} else if !strings.HasPrefix(resultURL, "http") {
			resultURL = "https://kns.cnki.net/kns8s/" + resultURL
		}

		results = append(results, CnkiResult{
			Title:		title,
			Authors:	strings.Join(authors, "; "),
			Source:		source,
			Date:		date,
			URL:		resultURL,
		})
	})

	return results, nil
}
