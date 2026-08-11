"""统一配置读写 + 凭据安全存储。

- 所有模块共用一份 config.json 读写逻辑
- 密码不再明文落盘：写入时用 AES-256-GCM 加密（密钥来自环境变量
  WOS_CONFIG_KEY 或首次自动生成的 config.key 文件，权限 600）
- 凭据支持环境变量注入（WOS_USERNAME / WOS_PASSWORD），优先于配置文件
"""
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
    if (_parent / "config.json").exists():
        BASE_DIR = _parent
    else:
        BASE_DIR = _self_dir

CONFIG_FILE = BASE_DIR / "config.json"
KEY_FILE = BASE_DIR / "config.key"

# 兼容旧版本中明文存储的字段名
LEGACY_PASSWORD_FIELDS = ("password", "wos_password", "cnki_password")

_key_cache = None


def load_config() -> dict:
    if CONFIG_FILE.exists():
        try:
            with open(CONFIG_FILE, 'r', encoding='utf-8') as f:
                return json.load(f)
        except Exception as e:
            print(f"Error loading config: {e}")
    return {}


def save_config(cfg: dict):
    try:
        with open(CONFIG_FILE, 'w', encoding='utf-8') as f:
            json.dump(cfg, f, indent=4)
    except Exception as e:
        print(f"Error saving to config: {e}")


# ─── 凭据加密 ────────────────────────────────────────────────────────────────

def _get_key() -> bytes:
    """获取 32 字节 AES 密钥：环境变量 WOS_CONFIG_KEY > config.key 文件 > 自动生成。"""
    global _key_cache
    if _key_cache:
        return _key_cache

    env_key = os.environ.get("WOS_CONFIG_KEY", "")
    if env_key:
        _key_cache = _hash_key(env_key)
        return _key_cache

    if KEY_FILE.exists():
        raw = KEY_FILE.read_bytes()
        if len(raw) == 32:
            _key_cache = raw
            return _key_cache

    # 首次运行：生成密钥文件
    _key_cache = get_random_bytes(32)
    try:
        KEY_FILE.write_bytes(_key_cache)
        os.chmod(KEY_FILE, 0o600)
        print(f"[config_util] 已生成配置加密密钥: {KEY_FILE}（请勿泄露/删除）")
    except Exception as e:
        print(f"[config_util] 写入密钥文件失败: {e}")
    return _key_cache


def _hash_key(secret: str) -> bytes:
    import hashlib
    return hashlib.sha256(secret.encode('utf-8')).digest()


def encrypt_secret(plain: str) -> str:
    """AES-256-GCM 加密，返回 base64(nonce + tag + ciphertext)。"""
    if not plain:
        return ""
    key = _get_key()
    nonce = get_random_bytes(12)
    cipher = AES.new(key, AES.MODE_GCM, nonce=nonce)
    ct, tag = cipher.encrypt_and_digest(plain.encode('utf-8'))
    return base64.b64encode(nonce + tag + ct).decode('ascii')


def decrypt_secret(token: str) -> str:
    """解密 encrypt_secret 的输出。token 非法时抛 ValueError。"""
    if not token:
        return ""
    key = _get_key()
    raw = base64.b64decode(token)
    if len(raw) < 28:
        raise ValueError("加密凭据格式错误")
    nonce, tag, ct = raw[:12], raw[12:28], raw[28:]
    cipher = AES.new(key, AES.MODE_GCM, nonce=nonce)
    return cipher.decrypt_and_verify(ct, tag).decode('utf-8')


# ─── 凭据读写 ────────────────────────────────────────────────────────────────

def _find_password(cfg: dict) -> str:
    """读取密码：优先加密字段，兼容旧版明文。"""
    for enc_field in ("password_enc", "wos_password_enc", "cnki_password_enc"):
        tok = cfg.get(enc_field)
        if tok:
            try:
                return decrypt_secret(tok)
            except Exception as e:
                print(f"[config_util] 密码解密失败（密钥可能已更换）: {e}")
                return ""
    for field in LEGACY_PASSWORD_FIELDS:
        if cfg.get(field):
            return cfg[field]
    return ""


def get_credentials(cfg: dict = None) -> tuple[str, str]:
    """返回 (username, password)。环境变量优先，其次配置文件。"""
    if cfg is None:
        cfg = load_config()

    username = os.environ.get("WOS_USERNAME") or cfg.get("username", "")
    password = os.environ.get("WOS_PASSWORD")
    if password is None:
        password = _find_password(cfg)
    return username, password


def set_password(cfg: dict, password: str) -> dict:
    """把密码以密文形式写入配置，并清理旧明文字段。"""
    cfg["password_enc"] = encrypt_secret(password)
    for field in LEGACY_PASSWORD_FIELDS:
        cfg.pop(field, None)
    return cfg


def set_credentials(cfg: dict, username: str, password: str) -> dict:
    cfg["username"] = username
    set_password(cfg, password)
    return cfg
