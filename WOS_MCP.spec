# -*- mode: python ; coding: utf-8 -*-


a = Analysis(
    ['wos_mcp\\server.py'],
    pathex=['wos_mcp'],
    binaries=[],
    datas=[],
    hiddenimports=['uvicorn', 'mcp', 'anyio', 'starlette', 'bs4', 'cv2'],
    hookspath=[],
    hooksconfig={},
    runtime_hooks=[],
    excludes=['pandas', 'matplotlib', 'PIL', 'lxml', 'scipy', 'PyQt5', 'PySide2', 'tkinter', 'sqlalchemy', 'IPython', 'pytest', 'unittest', 'PyQt6', 'PySide6'],
    noarchive=False,
    optimize=0,
)
pyz = PYZ(a.pure)

exe = EXE(
    pyz,
    a.scripts,
    a.binaries,
    a.datas,
    [],
    name='WOS_MCP',
    debug=False,
    bootloader_ignore_signals=False,
    strip=False,
    upx=True,
    upx_exclude=[],
    runtime_tmpdir=None,
    console=True,
    disable_windowed_traceback=False,
    argv_emulation=False,
    target_arch=None,
    codesign_identity=None,
    entitlements_file=None,
    icon=['icon.ico'],
)
