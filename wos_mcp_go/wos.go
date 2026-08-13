package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

var wosSessionLock sync.Mutex

func ensureWosSession() (string, map[string]string, error) {
	wosSessionLock.Lock()
	defer wosSessionLock.Unlock()

	cfg := LoadConfig()
	sid, _ := cfg["wos_sid"].(string)
	var cookies map[string]string
	if cookieMap, ok := cfg["wos_cookies"].(map[string]interface{}); ok {
		cookies = make(map[string]string)
		for k, v := range cookieMap {
			if vs, ok2 := v.(string); ok2 {
				cookies[k] = vs
			}
		}
	}

	if sid != "" && verifyWosSession(sid, cookies) {
		return sid, cookies, nil
	}

	fmt.Println("WOS session expired or not found, logging in...")
	username, password := GetCredentials(cfg)
	if username == "" || password == "" {
		return "", nil, errors.New("WOS credentials not configured in config.json or env")
	}

	client := NewWosLoginClient(username, password)
	newSid, newCookies, err := client.Login()
	if err != nil {
		return "", nil, err
	}

	cfg["wos_sid"] = newSid
	cfg["wos_cookies"] = newCookies
	SetCredentials(cfg, username, password)
	SaveConfig(cfg)

	return newSid, newCookies, nil
}

func verifyWosSession(sid string, cookies map[string]string) bool {
	url := fmt.Sprintf("https://www.webofscience.com/api/wosnx/core/runQuerySearch?SID=%s", sid)
	payload := map[string]interface{}{
		"product":	"ALLDB",
		"searchMode":	"general_semantic",
		"viewType":	"search",
		"serviceMode":	"summary",
		"search": map[string]interface{}{
			"mode":		"general_semantic",
			"database":	"ALLDB",
			"disableEdit":	false,
			"query":	[]map[string]interface{}{{"rowText": "TS=(test)"}},
			"display":	map[string]interface{}{"key": "nlp", "params": map[string]interface{}{"input": "test", "query_type": "Single-Term Concept"}},
			"count":	1,
		},
		"retrieve":	map[string]interface{}{"count": 1, "history": false, "locale": "en"},
	}

	data, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", url, bytes.NewBuffer(data))
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Origin", "https://www.webofscience.com")
	req.Header.Set("Referer", "https://www.webofscience.com/wos/alldb/smart-search")
	req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
	req.Header.Set("Accept", "application/x-ndjson, application/json, text/plain, */*")

	for k, v := range cookies {
		req.AddCookie(&http.Cookie{Name: k, Value: v})
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return false
	}

	body, _ := io.ReadAll(resp.Body)
	if strings.Contains(string(body), "Server.sessionExpired") {
		return false
	}
	return true
}

func parseWosResponse(body []byte) ([]map[string]interface{}, error) {
	var parsedData []map[string]interface{}

	lines := strings.Split(string(body), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var item map[string]interface{}
		if err := json.Unmarshal([]byte(line), &item); err == nil {
			parsedData = append(parsedData, item)
		}
	}

	if len(parsedData) == 0 {
		// try parse as single JSON array/object
		var single interface{}
		if err := json.Unmarshal(body, &single); err == nil {
			if m, ok := single.(map[string]interface{}); ok {
				parsedData = append(parsedData, m)
			} else if a, ok := single.([]interface{}); ok {
				for _, v := range a {
					if m, ok2 := v.(map[string]interface{}); ok2 {
						parsedData = append(parsedData, m)
					}
				}
			}
		}
	}
	return parsedData, nil
}
