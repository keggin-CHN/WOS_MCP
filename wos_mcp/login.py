import re
import random
import base64
import httpx
from bs4 import BeautifulSoup
from Crypto.Cipher import AES
from Crypto.Util.Padding import pad
import urllib.parse
CAS_HOST = 'https://uia.njfu.edu.cn'
IDP_HOST = 'https://idp-lib.njfu.edu.cn'
WOK_HOST = 'https://www.webofknowledge.com'
WOS_HOST = 'https://www.webofscience.com'
WOK_SP_ENTITY_ID = 'https://sp.tshhosting.com/shibboleth'
IDP_INITIATED_SSO_URL = f"{IDP_HOST}/idp/profile/SAML2/Unsolicited/SSO?providerId={urllib.parse.quote('https://sp.tshhosting.com/shibboleth')}&target={urllib.parse.quote(WOK_HOST + '/')}"
WAYFLESS_URL = f"{WOK_HOST}/?auth=ShibbolethIdPForm&entityID={urllib.parse.quote(IDP_HOST + '/idp/shibboleth')}&target={urllib.parse.quote(WOK_HOST + '/?DestApp=UA')}"
USER_AGENT = 'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36 Edg/151.0.0.0'
_CHARS = 'ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678'

def _rds(length: int) -> str:
    return ''.join((random.choice(_CHARS) for _ in range(length)))

def encrypt_password(password: str, aes_key: str) -> str:
    if not aes_key:
        return password
    random_prefix = _rds(64)
    random_iv = _rds(16)
    plaintext = (random_prefix + password).encode('utf-8')
    cipher = AES.new(aes_key.encode('utf-8'), AES.MODE_CBC, random_iv.encode('utf-8'))
    ciphertext = cipher.encrypt(pad(plaintext, AES.block_size))
    return base64.b64encode(ciphertext).decode('utf-8')

def _parse_auto_submit_form(html: str) -> tuple[str, dict]:
    soup = BeautifulSoup(html, 'html.parser')
    form = soup.find('form')
    if not form:
        raise RuntimeError('页面中没有找到表单')
    action = form.get('action', '')
    data = {}
    for inp in form.find_all('input'):
        name = inp.get('name')
        value = inp.get('value', '')
        type_ = inp.get('type', '').lower()
        if name and type_ not in ('submit', 'button', 'reset'):
            data[name] = value
    return (action, data)

class WosLogin:

    def __init__(self, username: str, password: str, verbose: bool=True):
        self.username = username
        self.password = password
        self.verbose = verbose
        self.session = httpx.Client(follow_redirects=True, timeout=30, headers={'User-Agent': USER_AGENT, 'Accept-Language': 'zh-CN,zh;q=0.9,en;q=0.8', 'Accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8'})
        self.sid = None

    def _log(self, msg: str):
        if self.verbose:
            print(msg)

    def _trigger_idp_sso(self) -> tuple[str, str]:
        self._log(f'[Step1] SP-initiated SSO (WAYFless): {WAYFLESS_URL[:120]}')
        resp = self.session.get(WAYFLESS_URL)
        if 'SAMLRequest' not in resp.text:
            raise RuntimeError('WAYFless URL 未返回 SAMLRequest 表单')
        self._log(f'[Step1] 获取到 SAMLRequest，自动提交至 IDP')
        action, form_data = _parse_auto_submit_form(resp.text)
        resp2 = self.session.post(action, data=form_data)
        final_url = str(resp2.url)
        self._log(f'[Step1] IDP 重定向后 URL: {final_url[:120]}')
        if 'uia.njfu.edu.cn' in final_url and 'login' in final_url:
            return (final_url, resp2.text)
        raise RuntimeError(f'SSO 触发失败，未到达 CAS 登录页。当前 URL: {final_url[:200]}')

    def _parse_login_page(self, login_url: str, html: str) -> dict:
        soup = BeautifulSoup(html, 'html.parser')
        form = soup.find(id='casLoginForm')
        if not form:
            raise RuntimeError('找不到 casLoginForm')
        action = form.get('action')
        from urllib.parse import urljoin
        post_url = urljoin(login_url, action) if action else login_url

        def find_input(name=None, id_=None):
            el = form.find('input', {'name': name}) if name else form.find('input', {'id': id_})
            return el['value'] if el and el.get('value') else None
        lt = find_input(name='lt')
        execution = find_input(name='execution')
        aes_key = find_input(id_='dynamicPwdEncryptSalt') or find_input(id_='pwdDefaultEncryptSalt')
        if not aes_key:
            m = re.search('pwdDefaultEncryptSalt\\s*=\\s*["\\\']([^"\\\']{8,32})["\\\']', html)
            if m:
                aes_key = m.group(1)
        self._log(f'[Step2] lt={str(lt)[:30]}..., execution={execution}, key={aes_key}')
        if not all([lt, execution, aes_key]):
            raise RuntimeError(f'解析登录页失败: lt={bool(lt)}, exec={bool(execution)}, key={bool(aes_key)}')
        return {'lt': lt, 'execution': execution, 'aes_key': aes_key, 'login_url': post_url}

    def _check_captcha(self) -> tuple[bool, str]:
        url = f'{CAS_HOST}/authserver/needCaptcha.html'
        resp = self.session.get(url, params={'username': self.username, 'pwdEncrypt2': 'pwdEncryptSalt', '_': str(random.randint(10 ** 12, 10 ** 13))})
        text = resp.text.strip()
        self._log(f'[Step3] needCaptcha API: {text}')
        need = False
        new_salt = None
        if '::::' in text:
            parts = text.split('::::')
            need = parts[0].lower() == 'true'
            new_salt = parts[1]
        else:
            need = text.lower() == 'true'
        self._log(f'[Step3] 需要验证码: {need}, 新盐: {new_salt}')
        return (need, new_salt)

    def _post_login(self, login_info: dict) -> str:
        encrypted_pwd = encrypt_password(self.password, login_info['aes_key'])
        self._log(f'[Step4] 提交登录 (密码已加密)')
        payload = {'username': self.username, 'password': encrypted_pwd, 'lt': login_info['lt'], 'dllt': 'userNamePasswordLogin', 'execution': login_info['execution'], '_eventId': 'submit', 'rmShown': '1'}
        import urllib.parse
        self._log(f'[Step4] PYTHON PAYLOAD: {urllib.parse.urlencode(payload)}')
        headers = {'Content-Type': 'application/x-www-form-urlencoded', 'Origin': CAS_HOST, 'Referer': login_info['login_url']}
        self._log(f"[Step4] PYTHON URL: {login_info['login_url']}")
        self._log(f'[Step4] PYTHON HEADERS: {headers}')
        self._log(f'[Step4] PYTHON COOKIES: {self.session.cookies}')
        resp = self.session.post(login_info['login_url'], data=payload, headers=headers)
        final_url = str(resp.url)
        self._log(f'[Step4] 登录后 URL: {final_url[:120]}')
        if 'uia.njfu.edu.cn' in final_url and '/login' in final_url:
            with open('login_failed.html', 'w', encoding='utf-8') as f:
                f.write(resp.text)
            soup = BeautifulSoup(resp.text, 'html.parser')
            msg = soup.find(id='msg')
            err_text = msg.text.strip() if msg else '未找到明确错误信息'
            raise RuntimeError(f'登录失败: {err_text}')
        return (final_url, resp)

    def _handle_consent(self, current_url: str, resp) -> tuple[str, object]:
        if 'idp-lib.njfu.edu.cn' not in current_url:
            return (current_url, resp)
        html = resp.text
        if '_shib_idp_consent' not in html and 'execution=e1s2' not in current_url:
            return (current_url, resp)
        self._log(f'[Step5-7] 处理 IDP 属性同意页')
        with open('consent.html', 'w', encoding='utf-8') as f:
            f.write(html)
        action, form_data = _parse_auto_submit_form(html)
        if not action.startswith('http'):
            action = IDP_HOST + action
        form_data.setdefault('_shib_idp_consentOptions', '_shib_idp_rememberConsent')
        form_data.setdefault('_eventId_proceed', 'Accept')
        resp2 = self.session.post(action, data=form_data)
        return (str(resp2.url), resp2)

    def _handle_saml_response(self, current_url: str, resp) -> str:
        if resp and 'SAMLResponse' in resp.text:
            self._log('[Step8] 检测到 SAMLResponse，自动提交')
            action, form_data = _parse_auto_submit_form(resp.text)
            if not action.startswith('http'):
                action = f"{WOK_HOST}/{action.lstrip('/')}"
            resp2 = self.session.post(action, data=form_data)
            current_url = str(resp2.url)
            self._log(f'[Step8] SAMLResponse 提交后 URL: {current_url[:120]}')
            for r in resp2.history:
                m = re.search('[?&]SID=([A-Za-z0-9]+)', str(r.url))
                if m:
                    self._log(f'[Step9] 从重定向历史中找到 SID: {m.group(1)}')
                    return m.group(1)
            return self._extract_sid(current_url, resp2.text)
        if resp:
            for r in getattr(resp, 'history', []):
                m = re.search('[?&]SID=([A-Za-z0-9]+)', str(r.url))
                if m:
                    self._log(f'[Step9] 从重定向历史中找到 SID: {m.group(1)}')
                    return m.group(1)
        return self._extract_sid(current_url, resp.text if resp else '')

    def _extract_sid(self, current_url: str, html: str='') -> str:
        m = re.search('[?&]SID=([A-Za-z0-9]+)', current_url)
        if m:
            self._log(f'[Step9] 找到 SID: {m.group(1)}')
            return m.group(1)
        self._log(f'[Step9] URL 中无 SID，继续请求: {current_url[:100]}')
        resp = self.session.get(current_url)
        final_url = str(resp.url)
        m = re.search('[?&]SID=([A-Za-z0-9]+)', final_url)
        if m:
            self._log(f'[Step9] 找到 SID: {m.group(1)}')
            return m.group(1)
        if resp:
            with open('sid_failed.html', 'w', encoding='utf-8') as f:
                f.write(resp.text)
        raise RuntimeError(f'无法提取 SID，最终 URL: {final_url[:200]}')

    def login(self) -> str:
        self._log('=== WoS 协议登录开始 ===')
        cas_login_url, html = self._trigger_idp_sso()
        login_info = self._parse_login_page(cas_login_url, html)
        need_captcha, new_salt = self._check_captcha()
        if need_captcha:
            raise RuntimeError('需要验证码，暂不支持自动处理。')
        if new_salt:
            login_info['aes_key'] = new_salt
        current_url, resp = self._post_login(login_info)
        current_url, resp = self._handle_consent(current_url, resp)
        sid = self._handle_saml_response(current_url, resp)
        self.sid = sid
        self._log(f'=== 登录成功！SID={sid} ===')
        return sid

    def get_cookies(self) -> dict:
        cookies = {}
        for cookie in self.session.cookies.jar:
            cookies[cookie.name] = cookie.value
        return cookies

    def close(self):
        self.session.close()

    def __enter__(self):
        return self

    def __exit__(self, *args):
        self.close()

def wos_login(username: str, password: str) -> tuple:
    with WosLogin(username, password) as client:
        sid = client.login()
        return (sid, client.get_cookies())
if __name__ == '__main__':
    import sys
    username = sys.argv[1] if len(sys.argv) > 1 else 'YOUR_USERNAME'
    password = sys.argv[2] if len(sys.argv) > 2 else 'YOUR_PASSWORD'
    try:
        sid, cookies = wos_login(username, password)
        print(f'\n登录成功！')
        print(f'SID: {sid}')
        test_url = f'https://www.webofscience.com/api/wosnx/core/getRecentHistoryForUser?SID={sid}'
        resp = httpx.get(test_url, cookies=cookies, timeout=15, headers={'User-Agent': USER_AGENT})
        print(f'API 验证状态码: {resp.status_code}')
    except Exception as e:
        print(f'\n登录失败: {e}')
        import traceback
        traceback.print_exc()