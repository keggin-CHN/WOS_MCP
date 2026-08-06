import time
import json
import base64
import requests
import cv2
import numpy as np
import re
from Crypto.Cipher import AES
from bs4 import BeautifulSoup
from login import WosLogin
import urllib.parse

class CnkiLogin(WosLogin):
    def _trigger_idp_sso(self) -> tuple[str, str]:
        provider_id = urllib.parse.quote('https://fsso.cnki.net/shibboleth')
        target = urllib.parse.quote('https://www.cnki.net')
        url = f'https://idp-lib.njfu.edu.cn/idp/profile/SAML2/Unsolicited/SSO?providerId={provider_id}&target={target}'
        self._log(f'[Step1] CNKI IDP-initiated SSO: {url}')
        resp = self.session.get(url)
        final_url = str(resp.url)
        self._log(f'[Step1] CNKI SSO triggered, current url: {final_url[:120]}')
        if 'uia.njfu.edu.cn' in final_url and 'login' in final_url:
            return final_url, resp.text
        raise RuntimeError(f'SSO failed, URL: {final_url}')

    def _handle_saml_response(self, current_url: str, resp) -> str:
        if resp and 'SAMLResponse' in resp.text:
            from login import _parse_auto_submit_form
            self._log('[Step8] Detected SAMLResponse, submitting to CNKI')
            action, form_data = _parse_auto_submit_form(resp.text)
            resp2 = self.session.post(action, data=form_data)
            self._log(f'[Step8] SAMLResponse submitted, URL: {resp2.url}')
            return "SUCCESS"
        self._log("No SAMLResponse found. Checking URL...")
        return "NO_SAML"
import sys
from pathlib import Path

if getattr(sys, 'frozen', False):
    BASE_DIR = Path(sys.executable).parent
else:
    _parent = Path(__file__).parent.parent
    _self_dir = Path(__file__).parent
    if (_parent / "config.json").exists():
        BASE_DIR = _parent
    else:
        BASE_DIR = _self_dir

CONFIG_FILE = BASE_DIR / "config.json"

def load_config() -> dict:
    if CONFIG_FILE.exists():
        try:
            with open(CONFIG_FILE, 'r', encoding='utf-8') as f:
                return json.load(f)
        except Exception:
            pass
    return {}

def save_config(cfg: dict):
    try:
        with open(CONFIG_FILE, 'w', encoding='utf-8') as f:
            json.dump(cfg, f, indent=4)
    except Exception as e:
        print(f"Error saving to config: {e}")

# ================= CAPTCHA SOLVER =================

def _solve_slider(bg_b64, slider_b64):
    bg_bytes = base64.b64decode(bg_b64)
    slider_bytes = base64.b64decode(slider_b64)
    
    bg_arr = np.frombuffer(bg_bytes, np.uint8)
    slider_arr = np.frombuffer(slider_bytes, np.uint8)
    
    bg_img = cv2.imdecode(bg_arr, cv2.IMREAD_COLOR)
    slider_img = cv2.imdecode(slider_arr, cv2.IMREAD_COLOR)
    
    bg_gray = cv2.cvtColor(bg_img, cv2.COLOR_BGR2GRAY)
    slider_gray = cv2.cvtColor(slider_img, cv2.COLOR_BGR2GRAY)
    
    bg_edges = cv2.Canny(bg_gray, 100, 200)
    slider_edges = cv2.Canny(slider_gray, 100, 200)
    
    non_zero = cv2.findNonZero(slider_edges)
    if non_zero is not None:
        x, y, w, h = cv2.boundingRect(non_zero)
        slider_edges = slider_edges[y:y+h, x:x+w]
        
    res = cv2.matchTemplate(bg_edges, slider_edges, cv2.TM_CCOEFF_NORMED)
    _, _, _, max_loc = cv2.minMaxLoc(res)
    
    return max_loc[0]

def _encrypt_aes(text, key):
    cipher = AES.new(key.encode('utf-8'), AES.MODE_ECB)
    pad_len = 16 - len(text) % 16
    padded = text + chr(pad_len) * pad_len
    encrypted = cipher.encrypt(padded.encode('utf-8'))
    return base64.b64encode(encrypted).decode('utf-8')

def _do_captcha_verify(session: requests.Session, ident: str, captcha_id: str) -> bool:
    print(f"Triggering CAPTCHA bypass for ident={ident}, captchaId={captcha_id}")
    url_get = "https://kns.cnki.net/verify-api/get"
    data_get = {
        "captchaType": "blockPuzzle",
        "clientUid": "slider-uuid-" + str(int(time.time()*1000)),
        "ident": ident,
        "captchaId": captcha_id,
        "ts": int(time.time() * 1000)
    }
    headers_get = {
        "Content-Type": "application/json;charset=UTF-8",
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64)",
        "Origin": "https://kns.cnki.net",
        "Referer": f"https://kns.cnki.net/verify/home?captchaType=blockPuzzle&ident={ident}&captchaId={captcha_id}"
    }
    resp = session.post(url_get, json=data_get, headers=headers_get)
    j = resp.json()
    if 'data' not in j or not j['data']:
        print("Failed to get captcha data:", j)
        return False
        
    rep = j['data']
    bg_b64 = rep['originalImageBase64']
    slider_b64 = rep['jigsawImageBase64']
    secret_key = rep.get('secretKey', '')
    token = rep['token']
    
    offset_x = _solve_slider(bg_b64, slider_b64)
    aes_key = secret_key if secret_key else token[:16]
    
    # Try exact offset and slightly adjusted
    for adj in [0, -1, 1, -2, 2]:
        x = offset_x + adj
        point_str = json.dumps({"x": x, "y": 5.0}, separators=(',', ':'))
        pointJson = _encrypt_aes(point_str, aes_key)
        
        url_check = "https://kns.cnki.net/verify-api/web/check"
        data_check = {
            "captchaType": "blockPuzzle",
            "pointJson": pointJson,
            "token": token,
            "ident": ident,
            "captchaId": captcha_id,
            "clientUid": "slider-uuid-" + str(int(time.time()*1000)),
            "ts": int(time.time() * 1000)
        }
        resp_check = session.post(url_check, json=data_check, headers=headers_get)
        j_check = resp_check.json()
        if j_check.get('success', False):
            print(f"CAPTCHA bypass success with offset {x}")
            return True
        time.sleep(1)
        
    print("CAPTCHA bypass failed after all adjustments.")
    return False

# ================= CLIENT =================

class CnkiClient:
    def __init__(self):
        self.session = requests.Session()
        self.session.headers.update({
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36 Edg/151.0.0.0"
        })
        
    def login(self, username, password):
        print("Authenticating CNKI via SSO...")
        with CnkiLogin(username, password) as client:
            cas_login_url, html = client._trigger_idp_sso()
            login_info = client._parse_login_page(cas_login_url, html)
            need_captcha, new_salt = client._check_captcha()
            if need_captcha:
                raise Exception("Need captcha during SSO login, aborting!")
            if new_salt:
                login_info['aes_key'] = new_salt
            
            current_url, resp = client._post_login(login_info)
            current_url, resp = client._handle_consent(current_url, resp)
            result = client._handle_saml_response(current_url, resp)
            if result != "SUCCESS":
                raise Exception(f"SAML Login failed: {result}")
            
            # Transfer cookies
            for cookie in client.session.cookies.jar:
                self.session.cookies.set(cookie.name, cookie.value, domain=cookie.domain, path=cookie.path)
            # Init KNS session
            self._safe_get("https://kns.cnki.net/kns8s/AdvSearch")
            
            # Save cookies
            cfg = load_config()
            cookie_dict = requests.utils.dict_from_cookiejar(self.session.cookies)
            cfg['cnki_cookies'] = cookie_dict
            cfg['username'] = username
            cfg['password'] = password
            save_config(cfg)
            
    def ensure_session(self):
        cfg = load_config()
        cookies = cfg.get("cnki_cookies", {})
        if cookies:
            self.session.cookies = requests.utils.cookiejar_from_dict(cookies)
            
            # simple verify
            resp = self._safe_get("https://kns.cnki.net/kns8s/AdvSearch", allow_redirects=True)
            if resp.status_code == 200 and 'verify/home' not in resp.url:
                return
                
        username = cfg.get("username", "")
        password = cfg.get("password", "")
        if not username or not password:
            raise ValueError("Credentials (username/password) not configured in config.json")
        self.login(username, password)
        
    def _solve_and_retry(self, resp, req_method, url, **kwargs):
        print(f"[{req_method}] Captcha triggered for URL: {url}")
        ident = None
        captcha_id = None
        if 'verify/home' in resp.url:
            import urllib.parse
            parsed = urllib.parse.urlparse(resp.url)
            qs = urllib.parse.parse_qs(parsed.query)
            ident = qs.get('ident', [''])[0]
            captcha_id = qs.get('captchaId', [''])[0]
        elif resp.status_code == 403 and 'captchaId' in resp.text:
            m1 = re.search(r'ident=([a-zA-Z0-9]+)', resp.text)
            m2 = re.search(r'captchaId=([a-zA-Z0-9\-]+)', resp.text)
            if m1 and m2:
                ident = m1.group(1)
                captcha_id = m2.group(1)
                
        if ident and captcha_id:
            if _do_captcha_verify(self.session, ident, captcha_id):
                if req_method.upper() == 'GET':
                    retry_url = f"{url}?captchaId={captcha_id}" if "?" not in url else f"{url}&captchaId={captcha_id}"
                    return self.session.request(req_method, retry_url, **kwargs)
                else:
                    return self.session.request(req_method, url, **kwargs)
        return resp
        
    def _safe_get(self, url, **kwargs):
        resp = self.session.get(url, **kwargs)
        if 'verify/home' in resp.url or resp.status_code == 403:
            resp = self._solve_and_retry(resp, 'GET', url, **kwargs)
        return resp
        
    def _safe_post(self, url, **kwargs):
        resp = self.session.post(url, **kwargs)
        if 'verify/home' in resp.url or resp.status_code == 403:
            resp = self._solve_and_retry(resp, 'POST', url, **kwargs)
        return resp
        
    def search(self, query: str, search_type: str = "SU", limit: int = 20) -> list:
        self.ensure_session()
        
        search_url = "https://kns.cnki.net/kns8s/brief/grid"
        
        # Map search_type if it is Chinese
        st_map = {
            "主题": "SU",
            "篇名": "TI",
            "全文": "KY",
            "作者": "AU",
            "机构": "AF"
        }
        st_code = st_map.get(search_type, search_type)
        
        query_json = {
            "Platform": "",
            "Resource": "CROSSDB",
            "Classid": "WD0FTY92",
            "Products": "",
            "QNode": {
                "QGroup": [
                    {
                        "Key": "Subject",
                        "Title": "",
                        "Logic": 0,
                        "Items": [
                            {
                                "Field": st_code,
                                "Value": query,
                                "Operator": "TOPRANK",
                                "Logic": 0,
                                "Title": "检索项"
                            }
                        ],
                        "ChildItems": []
                    }
                ]
            },
            "ExScope": 1,
            "SearchType": 2,
            "Rlang": "CHINESE",
            "KuaKuCode": "YSTT4HG0,LSTPFY1C,EMRPGLPA,JUP3MUPD,MPMFIG1A,WQ0UVIAA,BLZOG7CK,PWFIRAGL,NN3FJMUV,NLBO1Z6R",
            "Expands": {},
            "View": "changeDBCh",
            "SearchFrom": 1
        }
        
        import urllib.parse
        encoded_query_json = urllib.parse.quote(json.dumps(query_json, separators=(',', ':')))
        pages = 1 # Assuming limit <= 20 for simplicity
        
        raw_payload = f"boolSearch=true&QueryJson={encoded_query_json}&pageNum={pages}&pageSize={limit}&sortField=&sortType=&dstyle=listmode&productStr=&aside=&searchFrom=%E8%B5%84%E6%BA%90%E8%8C%83%E5%9B%B4%EF%BC%9A%E6%80%BB%E5%BA%93&subject=&language=&uniplatform=&CurPage={pages}"
        
        headers = {
            "Origin": "https://kns.cnki.net",
            "Referer": "https://kns.cnki.net/kns8s/defaultresult/index?kw=" + urllib.parse.quote(query),
            "Content-Type": "application/x-www-form-urlencoded; charset=UTF-8"
        }
        
        resp = self._safe_post(search_url, data=raw_payload, headers=headers)
        if resp.status_code != 200:
            print(f"Search failed with status: {resp.status_code}")
            return []
            
        soup = BeautifulSoup(resp.text, 'html.parser')
        rows = soup.select('table.result-table-list tbody tr')
        
        results = []
        for row in rows[:limit]:
            title_elem = row.select_one('td.name a')
            if not title_elem: continue
            
            title = title_elem.text.strip()
            href = title_elem.get('href', '')
            
            authors = [a.text.strip() for a in row.select('td.author a')]
            source = row.select_one('td.source a').text.strip() if row.select_one('td.source a') else ''
            date = row.select_one('td.date').text.strip() if row.select_one('td.date') else ''
            
            url = href
            if url.startswith('/'):
                url = "https://kns.cnki.net" + url
            elif not url.startswith('http'):
                url = "https://kns.cnki.net/kns8s/" + url
                
            results.append({
                "title": title,
                "authors": "; ".join(authors),
                "source": source,
                "date": date,
                "url": url
            })
            
        if not results:
            print("No results found. Dumping HTML for debugging...")
            with open("debug_search.html", "w", encoding="utf-8") as f:
                f.write(resp.text)
            
        return results

if __name__ == '__main__':
    client = CnkiClient()
    print("Testing search...")
    results = client.search("test", limit=5)
    print(f"Found {len(results)} results")
    for r in results:
        print(f"{r['title']} - {r['authors']} ({r['date']})")
        print(f"URL: {r['url']}")
