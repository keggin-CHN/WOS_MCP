import os
import json
import base64
import sys
from pathlib import Path
from Crypto.Cipher import AES
from Crypto.Random import get_random_bytes
if getattr(sys, 'frozen', False):
    BASE_DIR = Path(sys.executable).parent
else:
    _parent = Path(__file__).parent.parent
    _self_dir = Path(__file__).parent
    if (_parent / 'config.json').exists():
        BASE_DIR = _parent
    else:
        BASE_DIR = _self_dir
CONFIG_FILE = BASE_DIR / 'config.json'
KEY_FILE = BASE_DIR / 'config.key'
LEGACY_PASSWORD_FIELDS = ('password', 'wos_password', 'cnki_password')
_key_cache = None

def load_config() -> dict:
    if CONFIG_FILE.exists():
        try:
            with open(CONFIG_FILE, 'r', encoding='utf-8') as f:
                return json.load(f)
        except Exception as e:
            print(f'Error loading config: {e}')
    return {}

def save_config(cfg: dict):
    try:
        with open(CONFIG_FILE, 'w', encoding='utf-8') as f:
            json.dump(cfg, f, indent=4)
    except Exception as e:
        print(f'Error saving to config: {e}')

def _get_key() -> bytes:
    global _key_cache
    if _key_cache:
        return _key_cache
    env_key = os.environ.get('WOS_CONFIG_KEY', '')
    if env_key:
        _key_cache = _hash_key(env_key)
        return _key_cache
    if KEY_FILE.exists():
        raw = KEY_FILE.read_bytes()
        if len(raw) == 32:
            _key_cache = raw
            return _key_cache
    _key_cache = get_random_bytes(32)
    try:
        KEY_FILE.write_bytes(_key_cache)
        os.chmod(KEY_FILE, 384)
        print(f'[config_util] 已生成配置加密密钥: {KEY_FILE}（请勿泄露/删除）')
    except Exception as e:
        print(f'[config_util] 写入密钥文件失败: {e}')
    return _key_cache

def _hash_key(secret: str) -> bytes:
    import hashlib
    return hashlib.sha256(secret.encode('utf-8')).digest()

def encrypt_secret(plain: str) -> str:
    if not plain:
        return ''
    key = _get_key()
    nonce = get_random_bytes(12)
    cipher = AES.new(key, AES.MODE_GCM, nonce=nonce)
    ct, tag = cipher.encrypt_and_digest(plain.encode('utf-8'))
    return base64.b64encode(nonce + tag + ct).decode('ascii')

def decrypt_secret(token: str) -> str:
    if not token:
        return ''
    key = _get_key()
    raw = base64.b64decode(token)
    if len(raw) < 28:
        raise ValueError('加密凭据格式错误')
    nonce, tag, ct = (raw[:12], raw[12:28], raw[28:])
    cipher = AES.new(key, AES.MODE_GCM, nonce=nonce)
    return cipher.decrypt_and_verify(ct, tag).decode('utf-8')

def _find_password(cfg: dict) -> str:
    for enc_field in ('password_enc', 'wos_password_enc', 'cnki_password_enc'):
        tok = cfg.get(enc_field)
        if tok:
            try:
                return decrypt_secret(tok)
            except Exception as e:
                print(f'[config_util] 密码解密失败（密钥可能已更换）: {e}')
                return ''
    for field in LEGACY_PASSWORD_FIELDS:
        if cfg.get(field):
            return cfg[field]
    return ''

def get_credentials(cfg: dict=None) -> tuple[str, str]:
    if cfg is None:
        cfg = load_config()
    username = os.environ.get('WOS_USERNAME') or cfg.get('username', '')
    password = os.environ.get('WOS_PASSWORD')
    if password is None:
        password = _find_password(cfg)
    return (username, password)

def set_password(cfg: dict, password: str) -> dict:
    cfg['password_enc'] = encrypt_secret(password)
    for field in LEGACY_PASSWORD_FIELDS:
        cfg.pop(field, None)
    return cfg

def set_credentials(cfg: dict, username: str, password: str) -> dict:
    cfg['username'] = username
    set_password(cfg, password)
    return cfg