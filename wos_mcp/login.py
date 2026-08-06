import re
import random
import base64
import httpx
from bs4 import BeautifulSoup
from Crypto.Cipher import AES
from Crypto.Util.Padding import pad
import urllib.parse


# ─── 常量 ─────────────────────────────────────────────────────────────────────

CAS_HOST = 'https://uia.njfu.edu.cn'
IDP_HOST = 'https://idp-lib.njfu.edu.cn'
WOK_HOST = 'https://www.webofknowledge.com'
WOS_HOST = 'https://www.webofscience.com'

# IDP-initiated SSO 入口（绕过 access.clarivate.com SPA）
# 直接从 IDP 发起，SP entityID = WoK 的 Shibboleth SP
WOK_SP_ENTITY_ID = 'https://sp.tshhosting.com/shibboleth'
IDP_INITIATED_SSO_URL = (
    f'{IDP_HOST}/idp/profile/SAML2/Unsolicited/SSO?'
    f'providerId={urllib.parse.quote("https://sp.tshhosting.com/shibboleth")}&'
    f'target={urllib.parse.quote(WOK_HOST + "/")}'
)
WAYFLESS_URL = f'{WOK_HOST}/?auth=ShibbolethIdPForm&entityID={urllib.parse.quote(IDP_HOST + "/idp/shibboleth")}&target={urllib.parse.quote(WOK_HOST + "/?DestApp=UA")}'

USER_AGENT = (
    'Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 '
    '(KHTML, like Gecko) Chrome/151.0.0.0 Safari/537.36 Edg/151.0.0.0'
)

# ─── 加密工具 ────────────────────────────────────────────────────────────────

_CHARS = 'ABCDEFGHJKMNPQRSTWXYZabcdefhijkmnprstwxyz2345678'


def _rds(length: int) -> str:
    """生成指定长度的随机字符串"""
    return ''.join(random.choice(_CHARS) for _ in range(length))


def encrypt_password(password: str, aes_key: str) -> str:
    """
    AES-CBC-PKCS7 加密密码（与 JS 端 encryptAES 等价）
    """
    if not aes_key:
        return password
    
    random_prefix = _rds(64)
    random_iv = _rds(16)
    plaintext = (random_prefix + password).encode('utf-8')
    cipher = AES.new(aes_key.encode('utf-8'), AES.MODE_CBC, random_iv.encode('utf-8'))
    ciphertext = cipher.encrypt(pad(plaintext, AES.block_size))
    return base64.b64encode(ciphertext).decode('utf-8')



# ─── 辅助函数 ────────────────────────────────────────────────────────────────

def _parse_auto_submit_form(html: str) -> tuple[str, dict]:
    """
    解析 HTML 中的自动提交表单，返回 (action_url, form_data)
    """
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
    return action, data


# ─── 核心登录类 ───────────────────────────────────────────────────────────────

class WosLogin:
    """
    南京林业大学 → Web of Science 协议登录客户端

    使用示例:
        login = WosLogin('YOUR_USERNAME', 'YOUR_PASSWORD')
        sid = login.login()
        print('SID:', sid)
    """

    def __init__(self, username: str, password: str, verbose: bool = True):
        self.username = username
        self.password = password
        self.verbose = verbose
        self.session = httpx.Client(
            follow_redirects=True,
            timeout=30,
            headers={
                'User-Agent': USER_AGENT,
                'Accept-Language': 'zh-CN,zh;q=0.9,en;q=0.8',
                'Accept': 'text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8',
            },
        )
        self.sid = None

    def _log(self, msg: str):
        if self.verbose:
            print(msg)

    # ── Step 1: 触发认证链 → 到达 CAS 登录页 ────────────────────────────────
    def _trigger_idp_sso(self) -> tuple[str, str]:
        """
        使用 SP-initiated SSO (WAYFless URL) 触发 SAML 流程，
        获取 SAMLRequest 并提交给 IDP，最终跟随重定向到达 CAS 登录页。
        返回 (final_url, html_content)
        """
        self._log(f'[Step1] SP-initiated SSO (WAYFless): {WAYFLESS_URL[:120]}')
        resp = self.session.get(WAYFLESS_URL)
        
        if 'SAMLRequest' not in resp.text:
            raise RuntimeError('WAYFless URL 未返回 SAMLRequest 表单')
            
        self._log(f'[Step1] 获取到 SAMLRequest，自动提交至 IDP')
        action, form_data = _parse_auto_submit_form(resp.text)
        
        # 提交 SAMLRequest
        resp2 = self.session.post(action, data=form_data)
        final_url = str(resp2.url)
        self._log(f'[Step1] IDP 重定向后 URL: {final_url[:120]}')

        if 'uia.njfu.edu.cn' in final_url and 'login' in final_url:
            return final_url, resp2.text

        raise RuntimeError(f'SSO 触发失败，未到达 CAS 登录页。当前 URL: {final_url[:200]}')

    # ── Step 2: 解析 CAS 登录页，获取 lt / execution / AES key ──────────────
    def _parse_login_page(self, login_url: str, html: str) -> dict:
        """解析 CAS 登录页，提取 lt、execution、AES 密钥"""
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
        aes_key = (find_input(id_='dynamicPwdEncryptSalt')
                   or find_input(id_='pwdDefaultEncryptSalt'))

        if not aes_key:
            m = re.search(r'pwdDefaultEncryptSalt\s*=\s*["\']([^"\']{8,32})["\']', html)
            if m:
                aes_key = m.group(1)

        self._log(f'[Step2] lt={str(lt)[:30]}..., execution={execution}, key={aes_key}')

        if not all([lt, execution, aes_key]):
            raise RuntimeError(f'解析登录页失败: lt={bool(lt)}, exec={bool(execution)}, key={bool(aes_key)}')

        return {
            'lt': lt,
            'execution': execution,
            'aes_key': aes_key,
            'login_url': post_url,
        }

    # ── Step 3: 验证码检查 ────────────────────────────────────────────────────
    def _check_captcha(self) -> tuple[bool, str]:
        url = f'{CAS_HOST}/authserver/needCaptcha.html'
        resp = self.session.get(url, params={
            'username': self.username,
            'pwdEncrypt2': 'pwdEncryptSalt',
            '_': str(random.randint(10**12, 10**13)),
        })
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
        return need, new_salt

    # ── Step 4: POST 登录 → 获取 CAS Service Ticket ──────────────────────────
    def _post_login(self, login_info: dict) -> str:
        """提交 CAS 表单，返回认证完成后的当前 URL"""
        encrypted_pwd = encrypt_password(self.password, login_info['aes_key'])
        self._log(f'[Step4] 提交登录 (密码已加密)')

        resp = self.session.post(
            login_info['login_url'],
            data={
                'username': self.username,
                'password': encrypted_pwd,
                'lt': login_info['lt'],
                'dllt': 'userNamePasswordLogin',
                'execution': login_info['execution'],
                '_eventId': 'submit',
                'rmShown': '1',
            },
            headers={
                'Content-Type': 'application/x-www-form-urlencoded',
                'Origin': CAS_HOST,
                'Referer': login_info['login_url'],
            },
        )

        final_url = str(resp.url)
        self._log(f'[Step4] 登录后 URL: {final_url[:120]}')

        if 'uia.njfu.edu.cn' in final_url and '/login' in final_url:
            # 检查错误信息
            with open('login_failed.html', 'w', encoding='utf-8') as f:
                f.write(resp.text)
            soup = BeautifulSoup(resp.text, 'html.parser')
            msg = soup.find(id='msg')
            err_text = msg.text.strip() if msg else '未找到明确错误信息'
            raise RuntimeError(f'登录失败: {err_text}')
        return final_url, resp

    # ── Step 5-7: 处理 IDP 同意页 ─────────────────────────────────────────────
    def _handle_consent(self, current_url: str, resp) -> tuple[str, object]:
        """
        如果 IDP 显示属性释放同意页，自动提交同意。
        返回 (当前URL, 最新response)
        """
        if 'idp-lib.njfu.edu.cn' not in current_url:
            return current_url, resp

        html = resp.text
        if '_shib_idp_consent' not in html and 'execution=e1s2' not in current_url:
            return current_url, resp

        self._log(f'[Step5-7] 处理 IDP 属性同意页')
        with open('consent.html', 'w', encoding='utf-8') as f:
            f.write(html)
        action, form_data = _parse_auto_submit_form(html)

        if not action.startswith('http'):
            action = IDP_HOST + action

        form_data.setdefault('_shib_idp_consentOptions', '_shib_idp_rememberConsent')
        form_data.setdefault('_eventId_proceed', 'Accept')

        resp2 = self.session.post(action, data=form_data)
        return str(resp2.url), resp2

    # ── Step 8-9: POST SAMLResponse → 获取 SID ───────────────────────────────
    def _handle_saml_response(self, current_url: str, resp) -> str:
        """
        如果当前页面是 SAMLResponse 自动提交页，提交它并提取 SID。
        否则直接从 URL 提取 SID。
        """
        # 如果响应体里有 SAMLResponse 表单（IDP → SP 的最后一步）
        if resp and 'SAMLResponse' in resp.text:
            self._log('[Step8] 检测到 SAMLResponse，自动提交')
            action, form_data = _parse_auto_submit_form(resp.text)
            if not action.startswith('http'):
                # 通常是 https://www.webofknowledge.com/?auth=Shibboleth
                action = f'{WOK_HOST}/{action.lstrip("/")}'
            resp2 = self.session.post(action, data=form_data)
            current_url = str(resp2.url)
            self._log(f'[Step8] SAMLResponse 提交后 URL: {current_url[:120]}')
            
            # Check redirect history for SID
            for r in resp2.history:
                m = re.search(r'[?&]SID=([A-Za-z0-9]+)', str(r.url))
                if m:
                    self._log(f'[Step9] 从重定向历史中找到 SID: {m.group(1)}')
                    return m.group(1)
            
            return self._extract_sid(current_url, resp2.text)

        # Check resp history as well if we didn't POST SAMLResponse
        if resp:
            for r in getattr(resp, 'history', []):
                m = re.search(r'[?&]SID=([A-Za-z0-9]+)', str(r.url))
                if m:
                    self._log(f'[Step9] 从重定向历史中找到 SID: {m.group(1)}')
                    return m.group(1)

        return self._extract_sid(current_url, resp.text if resp else '')

    def _extract_sid(self, current_url: str, html: str = '') -> str:
        """从 URL 中提取 SID"""
        m = re.search(r'[?&]SID=([A-Za-z0-9]+)', current_url)
        if m:
            self._log(f'[Step9] 找到 SID: {m.group(1)}')
            return m.group(1)

        # 如果还没有 SID，继续跟随重定向
        self._log(f'[Step9] URL 中无 SID，继续请求: {current_url[:100]}')
        resp = self.session.get(current_url)
        final_url = str(resp.url)
        m = re.search(r'[?&]SID=([A-Za-z0-9]+)', final_url)
        if m:
            self._log(f'[Step9] 找到 SID: {m.group(1)}')
            return m.group(1)
        
        if resp:
            with open('sid_failed.html', 'w', encoding='utf-8') as f:
                f.write(resp.text)

        raise RuntimeError(f'无法提取 SID，最终 URL: {final_url[:200]}')

    # ── 主流程 ────────────────────────────────────────────────────────────────
    def login(self) -> str:
        """执行完整登录，返回 WoS SID"""
        self._log('=== WoS 协议登录开始 ===')

        # Step 1: IDP 触发 SSO
        cas_login_url, html = self._trigger_idp_sso()

        # Step 2: 解析登录页
        login_info = self._parse_login_page(cas_login_url, html)

        # 3. 检查验证码并获取新加密盐（如有）
        need_captcha, new_salt = self._check_captcha()
        if need_captcha:
            raise RuntimeError('需要验证码，暂不支持自动处理。')
        if new_salt:
            login_info['aes_key'] = new_salt

        # 4. 提交登录表单
        current_url, resp = self._post_login(login_info)

        # Step 5-7: 处理 IDP 同意页
        current_url, resp = self._handle_consent(current_url, resp)

        # Step 8-9: POST SAMLResponse → 提取 SID
        sid = self._handle_saml_response(current_url, resp)

        self.sid = sid
        self._log(f'=== 登录成功！SID={sid} ===')
        return sid

    def get_cookies(self) -> dict:
        """获取所有 Cookie 以供后续接口调用"""
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


# ─── 快捷函数 ─────────────────────────────────────────────────────────────────

def wos_login(username: str, password: str) -> tuple:
    """
    一键登录，返回 (sid, cookies)。
    
    Example:
        sid, cookies = wos_login('YOUR_USERNAME', 'YOUR_PASSWORD')
    """
    with WosLogin(username, password) as client:
        sid = client.login()
        return sid, client.get_cookies()


# ─── 测试入口 ─────────────────────────────────────────────────────────────────

if __name__ == '__main__':
    import sys

    username = sys.argv[1] if len(sys.argv) > 1 else 'YOUR_USERNAME'
    password = sys.argv[2] if len(sys.argv) > 2 else 'YOUR_PASSWORD'

    try:
        sid, cookies = wos_login(username, password)
        print(f'\n登录成功！')
        print(f'SID: {sid}')

        # 验证 session
        test_url = f'https://www.webofscience.com/api/wosnx/core/getRecentHistoryForUser?SID={sid}'
        resp = httpx.get(test_url, cookies=cookies, timeout=15,
                         headers={'User-Agent': USER_AGENT})
        print(f'API 验证状态码: {resp.status_code}')

    except Exception as e:
        print(f'\n登录失败: {e}')
        import traceback
        traceback.print_exc()
