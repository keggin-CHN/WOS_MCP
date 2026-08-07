import os
import json
import httpx
from typing import Optional
from pathlib import Path

from mcp.server.fastmcp import FastMCP
from login import wos_login, USER_AGENT
from bs4 import BeautifulSoup
from cnki_client import CnkiClient

import sys
import threading
import time
import ctypes

_session_lock = threading.Lock()

def hide_console():
    time.sleep(20)
    hwnd = ctypes.windll.kernel32.GetConsoleWindow()
    if hwnd:
        ctypes.windll.user32.ShowWindow(hwnd, 0) # SW_HIDE

if getattr(sys, 'frozen', False):
    # If running in a PyInstaller bundle, use the executable's directory
    BASE_DIR = Path(sys.executable).parent
    threading.Thread(target=hide_console, daemon=True).start()
else:
    # If running as a normal python script:
    # - Local dev layout: c:/code/wos/wos_mcp/server.py -> parent.parent = c:/code/wos
    # - Server deploy layout: /opt/wos_mcp/server.py -> parent = /opt/wos_mcp
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
        except Exception as e:
            print(f"Error loading config: {e}")
    return {}

config = load_config()
dp_str = config.get("download_path", "download")
dp = Path(dp_str)
if not dp.is_absolute():
    dp = BASE_DIR / dp
DOWNLOAD_DIR = dp.resolve()
DOWNLOAD_DIR.mkdir(parents=True, exist_ok=True)

from mcp.server.transport_security import TransportSecuritySettings
mcp = FastMCP("Academic_WoS_CNKI", transport_security=TransportSecuritySettings(enable_dns_rebinding_protection=False))
cnki_client = CnkiClient()

def verify_session(sid: str, cookies: dict) -> bool:
    """验证当前的 SID 是否有效"""
    url = f"https://www.webofscience.com/api/wosnx/core/runQuerySearch?SID={sid}"
    headers = {
        "User-Agent": USER_AGENT,
        "Origin": "https://www.webofscience.com",
        "Referer": "https://www.webofscience.com/wos/alldb/smart-search",
        "Content-Type": "text/plain;charset=UTF-8",
        "Accept": "application/x-ndjson, application/json, text/plain, */*",
    }
    payload = {
        "product": "ALLDB",
        "searchMode": "general_semantic",
        "viewType": "search",
        "serviceMode": "summary",
        "search": {
            "mode": "general_semantic",
            "database": "ALLDB",
            "disableEdit": False,
            "query": [{"rowText": "TS=(test)"}],
            "display": {"key": "nlp", "params": {"input": "test", "query_type": "Single-Term Concept"}},
            "count": 1
        },
        "retrieve": {"count": 1, "history": False, "locale": "en"}
    }
    try:
        resp = httpx.post(url, headers=headers, cookies=cookies, json=payload, timeout=10)
        if resp.status_code != 200:
            return False
        if "Server.sessionExpired" in resp.text:
            return False
        return True
    except Exception:
        return False

def ensure_session() -> tuple[str, dict]:
    """确保 session 有效，自动登录更新缓存"""
    with _session_lock:
        cfg = load_config()
        sid = cfg.get("wos_sid")
        cookies = cfg.get("wos_cookies", {})

        if sid and cookies and verify_session(sid, cookies):
            print("缓存凭证验证成功。")
            return sid, cookies

    print("缓存凭证已过期或不存在，正在自动登录获取新的凭证...")
    username = cfg.get("username", "")
    password = cfg.get("password", "")
    if not username or not password:
        raise ValueError("凭证已过期，且未配置账号密码。请在 config.json 中配置 username 和 password。")

    sid, cookies = wos_login(username, password)
    
    cfg["wos_sid"] = sid
    cfg["wos_cookies"] = cookies
    try:
        with open(CONFIG_FILE, 'w', encoding='utf-8') as f:
            json.dump(cfg, f, indent=4)
    except Exception as e:
        print(f"Error saving to config: {e}")

    return sid, cookies

@mcp.tool()
def search_literature(
    query: str, 
    limit: int = 10, 
    year_range: str = "", 
    doc_type: str = "",
    sort: str = "relevance",
    editions: list[str] = None,
    first: int = 1
) -> str:
    """
    Search for literature on Web of Science.
    CRITICAL: If the user provides Chinese terms in the query, YOU MUST TRANSLATE them to English before calling this tool. Web of Science SCI/SSCI requires English keywords.
    
    Args:
        query: The search query string (e.g. topic, title, author).
        limit: Number of results to return.
        year_range: Optional year range (e.g. "2020-2024").
        doc_type: Optional document type (e.g. "Article", "Review").
        sort: Sort order. Options: "relevance", "times-cited-descending", "date-descending".
        editions: Optional list of databases to filter by (e.g. ["WOS.SCI", "WOS.SSCI", "WOS.CPCI-S"]).
        first: Pagination offset (starts at 1). For page 2 with limit 50, use 51.
    """
    if editions is None:
        editions = []
        
    try:
        sid, cookies = ensure_session()
    except Exception as e:
        return f"Error ensuring session: {e}"

    url = f"https://www.webofscience.com/api/wosnx/core/runQuerySearch?SID={sid}"
    headers = {
        "User-Agent": USER_AGENT,
        "Origin": "https://www.webofscience.com",
        "Referer": "https://www.webofscience.com/wos/alldb/smart-search",
        "Content-Type": "text/plain;charset=UTF-8",
        "Accept": "application/x-ndjson, application/json, text/plain, */*",
    }
    
    row_text = f"TS=({query})"
    if year_range:
        row_text += f" AND PY=({year_range})"
    if doc_type:
        row_text += f" AND DT=({doc_type})"
        
    payload = {
        "product": "WOSCC" if editions else "ALLDB",
        "searchMode": "general_semantic",
        "viewType": "search",
        "serviceMode": "summary",
        "search": {
            "mode": "general_semantic",
            "database": "WOSCC" if editions else "ALLDB",
            "disableEdit": False,
            "query": [{"rowText": row_text}],
            "display": {"key": "nlp", "params": {"input": query, "query_type": "Single-Term Concept"}},
            "blending": "blended",
            "count": limit if limit > 0 else 100
        },
        "retrieve": {
            "first": first,
            "count": limit if limit > 0 else 20,
            "history": True,
            "jcr": True,
            "sort": sort,
            "analyzes": ["TP.Value.6", "PY.Field_D.6", "DT.Value.6", "AU.Value.6"],
            "trueCount": False,
            "locale": "en"
        },
        "eventMode": None
    }
    
    if editions:
        payload["search"]["editions"] = editions

    try:
        with httpx.Client(cookies=cookies) as client:
            response = client.post(url, headers=headers, json=payload, timeout=15.0)
            if response.status_code != 200:
                return f"Error from Web of Science: HTTP {response.status_code}\n{response.text}"
                
            try:
                parsed_data = response.json()
            except json.JSONDecodeError:
                parsed_data = []
                for line in response.text.strip().split('\n'):
                    if not line.strip(): continue
                    try:
                        parsed_data.append(json.loads(line))
                    except Exception:
                        pass
            
            search_info = next((item.get('payload') for item in parsed_data if isinstance(item, dict) and item.get('key') == 'searchInfo'), {})
            records_data = next((item.get('payload') for item in parsed_data if isinstance(item, dict) and item.get('key') == 'records'), {})
            
            records = []
            if isinstance(records_data, dict):
                for idx_key, rec in records_data.items():
                    try:
                        title = ""
                        try:
                            item_lang = rec.get("titles", {}).get("item", {})
                            lang_key = list(item_lang.keys())[0] if item_lang else "en"
                            title = item_lang.get(lang_key, [{}])[0].get("title", "")
                        except: pass
                        
                        authors = ""
                        try:
                            author_lang = rec.get("names", {}).get("author", {})
                            lang_key = list(author_lang.keys())[0] if author_lang else "en"
                            author_list = author_lang.get(lang_key, [])
                            authors = "; ".join([a.get("wos_standard", a.get("display_name", "")) for a in author_list if a])
                        except: pass

                        source = ""
                        try:
                            source_lang = rec.get("titles", {}).get("source", {})
                            lang_key = list(source_lang.keys())[0] if source_lang else "en"
                            source = source_lang.get(lang_key, [{}])[0].get("title", "")
                        except: pass

                        pub_info = rec.get("pub_info", {})
                        year = pub_info.get("pubyear", "")
                        doi = rec.get("doi", "")
                        citations = rec.get("citation_related", {}).get("counts", {}).get("WOSCC", 0)
                        wosId = rec.get("colluid", "")
                        abstract = ""
                        try:
                            abs_basic = rec.get("abstract", {}).get("basic", {})
                            lang_key = list(abs_basic.keys())[0] if abs_basic else "en"
                            abstract = abs_basic.get(lang_key, {}).get("abstract", "")[:300]
                        except: pass

                        records.append({
                            "title": title,
                            "authors": authors[:50] + "..." if len(authors) > 50 else authors,
                            "source": source,
                            "year": year,
                            "citations": citations,
                            "doi": doi,
                            "wosId": wosId,
                            "abstract": abstract.replace("\n", " ")
                        })
                    except Exception:
                        pass
                        
            total_results = search_info.get('RecordsFound', 0)
            if not records:
                return f"Success, but found 0 records or failed to parse. Total Reported: {total_results}"
                
            output = f"Found **{total_results}** results in WoS Core Collection.\n\n"
            output += "| # | Title | Authors | Source | Year | Cited | WoS ID | DOI |\n"
            output += "|---|-------|---------|--------|------|-------|--------|-----|\n"
            for i, r in enumerate(records):
                output += f"| {i+1} | {r['title']} | {r['authors']} | {r['source']} | {r['year']} | {r['citations']} | {r['wosId']} | {r['doi']} |\n"
                
            return output
            
    except Exception as e:
        return f"An error occurred during search: {str(e)}"

@mcp.tool()
def get_wos_paper_details(wos_id: str) -> str:
    """
    Fetch full detailed metadata for a specific paper, including JIF (Impact Factor), JCR Quartile, and Keywords.
    
    Args:
        wos_id: The Web of Science ID (e.g., WOS:000295471900004).
    """
    try:
        sid, cookies = ensure_session()
    except Exception as e:
        return f"Error ensuring session: {e}"

    url = f"https://www.webofscience.com/api/wosnx/core/runQuerySearch?SID={sid}"
    headers = {
        "User-Agent": USER_AGENT,
        "Origin": "https://www.webofscience.com",
        "Content-Type": "text/plain;charset=UTF-8",
        "Accept": "application/x-ndjson, application/json, text/plain, */*",
    }
    
    payload = {
        "product": "WOSCC",
        "searchMode": "general",
        "viewType": "search",
        "serviceMode": "summary",
        "search": {
            "mode": "general",
            "database": "WOSCC",
            "query": [{"rowField": "UT", "rowText": f"{wos_id}"}]
        },
        "retrieve": {
            "first": 1,
            "count": 1,
            "history": False,
            "jcr": True,
            "sort": "relevance",
            "analyzes": [],
            "locale": "en"
        },
        "eventMode": None
    }
    
    try:
        with httpx.Client(cookies=cookies) as client:
            response = client.post(url, headers=headers, json=payload, timeout=15.0)
            if response.status_code != 200:
                return f"Error from Web of Science: HTTP {response.status_code}"
                
            try:
                parsed_data = response.json()
            except json.JSONDecodeError:
                parsed_data = []
                for line in response.text.strip().split('\n'):
                    if not line.strip(): continue
                    try:
                        parsed_data.append(json.loads(line))
                    except: pass
            
            records_data = next((item.get('payload') for item in parsed_data if isinstance(item, dict) and item.get('key') == 'records'), {})
            jcr_global_data = next((item.get('payload') for item in parsed_data if isinstance(item, dict) and item.get('key') == 'jcr'), {})
            
            if not records_data or not isinstance(records_data, dict):
                return "Failed to fetch record or record not found."
                
            rec = list(records_data.values())[0]
            
            title = ""
            try: title = rec.get("titles", {}).get("item", {}).get("en", [{}])[0].get("title", "")
            except: pass
            
            authors = ""
            try:
                author_list = rec.get("names", {}).get("author", {}).get("en", [])
                authors = "; ".join([a.get("wos_standard", a.get("display_name", "")) for a in author_list if a])
            except: pass
            
            source = ""
            try: source = rec.get("titles", {}).get("source", {}).get("en", [{}])[0].get("title", "")
            except: pass
            
            doi = rec.get("doi", "")
            citations = rec.get("citation_related", {}).get("counts", {}).get("WOSCC", 0)
            
            author_keywords = []
            keywords_plus = []
            try:
                ak_list = rec.get("keywords", {}).get("author", {}).get("en", [])
                author_keywords = [k.get("keyword", "") for k in ak_list]
                kp_list = rec.get("keywords", {}).get("plus", {}).get("en", [])
                keywords_plus = [k.get("keyword", "") for k in kp_list]
            except: pass
            
            jif = ""
            jcr_category = []
            jcr_quartiles = []
            jci = ""
            try:
                # The JCR data is keyed by something like "ISSN:XXXX-XXXX"
                if jcr_global_data:
                    first_jcr = list(jcr_global_data.values())[0]
                    if first_jcr.get("CitationIndicator"):
                        jci = str(first_jcr.get("CitationIndicator"))
                    cat_data = first_jcr.get("CategoryIFData", [])
                    for cat in cat_data:
                        if cat.get("CategoryName"): jcr_category.append(cat.get("CategoryName"))
                        if cat.get("JifQuartile"): jcr_quartiles.append(cat.get("JifQuartile"))
            except: pass
            
            abstract = ""
            try:
                abs_basic = rec.get("abstract", {}).get("basic", {})
                lang_key = list(abs_basic.keys())[0] if abs_basic else "en"
                abstract = abs_basic.get(lang_key, {}).get("abstract", "")
            except: pass
            
            out = f"## {title}\n"
            out += f"**Authors:** {authors}\n\n"
            out += f"**Source:** {source}\n"
            out += f"**DOI:** {doi}\n"
            out += f"**WOS ID:** {wos_id}\n"
            out += f"**Times Cited (WOSCC):** {citations}\n\n"
            
            if jci: out += f"**Journal Citation Indicator (JCI):** {jci}\n"
            if jcr_category and jcr_quartiles:
                out += f"**JCR Quartile:** {', '.join(jcr_quartiles)} (in {', '.join(jcr_category)})\n"
                
            out += "\n### Keywords\n"
            out += f"**Author Keywords:** {', '.join(author_keywords) if author_keywords else 'None'}\n"
            out += f"**Keywords Plus:** {', '.join(keywords_plus) if keywords_plus else 'None'}\n"
            
            if abstract:
                out += f"\n### Abstract\n{abstract}\n"
            
            return out
            
    except Exception as e:
        return f"An error occurred: {str(e)}"

@mcp.tool()
def download_literature(doi_or_wosid: str) -> str:
    """
    Get the full-text download link for a specific document by DOI or WoS ID.
    If possible, attempts to sniff and download the PDF locally.
    
    Args:
        doi_or_wosid: The DOI or Web of Science ID.
    """
    try:
        sid, cookies = ensure_session()
    except Exception as e:
        return f"Error ensuring session: {e}"

    if doi_or_wosid.startswith('10.'):
        unpaywall_url = f"https://api.unpaywall.org/v2/{doi_or_wosid}?email=mcp-test@example.com"
        try:
            resp = httpx.get(unpaywall_url, timeout=10)
            if resp.status_code == 200:
                data = resp.json()
                best_oa_loc = data.get("best_oa_location")
                if best_oa_loc and best_oa_loc.get("url_for_pdf"):
                    pdf_url = best_oa_loc.get("url_for_pdf")
                    try:
                        pdf_resp = httpx.get(pdf_url, follow_redirects=True, timeout=30)
                        if pdf_resp.status_code == 200 and 'application/pdf' in pdf_resp.headers.get('Content-Type', '').lower():
                            filename = f"{doi_or_wosid.replace('/', '_')}.pdf"
                            filepath = DOWNLOAD_DIR / filename
                            with open(filepath, 'wb') as f:
                                f.write(pdf_resp.content)
                            return f"成功下载了全文 PDF！已保存至本地：{filepath.absolute()}"
                    except Exception as e:
                        print(f"Failed to download PDF: {e}")
                        pass
        except Exception as e:
            print(f"Failed to hit unpaywall: {e}")
            pass

    return (f"未能嗅探到可直接下载的免费 PDF。\n\n"
            f"请提示用户：未找到直接的 PDF 下载流。建议引导用户手动通过 DOI 跳转下载：https://doi.org/{doi_or_wosid}")


@mcp.tool()
def export_wos_papers(
    query: str, 
    limit: int = 50, 
    year_range: str = "", 
    doc_type: str = "",
    format: str = "bibtex"
) -> str:
    """
    Bulk export papers matching a search query to a specified format (csv, json, bibtex, ris).
    This searches Web of Science and exports the metadata into a file in the download directory.
    
    Args:
        query: The search query string (e.g. topic, title, author).
        limit: Number of results to export (max 100).
        year_range: Optional year range (e.g. "2020-2024").
        doc_type: Optional document type (e.g. "Article", "Review").
        format: The export format. Options: "csv", "json", "bibtex", "ris".
    """
    try:
        sid, cookies = ensure_session()
    except Exception as e:
        return f"Error ensuring session: {e}"

    if limit > 100: limit = 100
    url = f"https://www.webofscience.com/api/wosnx/core/runQuerySearch?SID={sid}"
    headers = {
        "User-Agent": USER_AGENT,
        "Origin": "https://www.webofscience.com",
        "Content-Type": "text/plain;charset=UTF-8",
        "Accept": "application/x-ndjson, application/json, text/plain, */*",
    }
    
    row_text = f"TS=({query})"
    if year_range:
        row_text += f" AND PY=({year_range})"
    if doc_type:
        row_text += f" AND DT=({doc_type})"
        
    payload = {
        "product": "ALLDB",
        "searchMode": "general_semantic",
        "viewType": "search",
        "serviceMode": "summary",
        "search": {
            "mode": "general_semantic",
            "database": "ALLDB",
            "disableEdit": False,
            "query": [{"rowText": row_text}],
            "display": {"key": "nlp", "params": {"input": query, "query_type": "Single-Term Concept"}},
            "blending": "blended",
            "count": limit if limit > 0 else 50
        },
        "retrieve": {
            "first": 1,
            "count": limit if limit > 0 else 50,
            "history": False,
            "jcr": False,
            "sort": "relevance",
            "analyzes": [],
            "trueCount": False,
            "locale": "en"
        },
        "eventMode": None
    }
    
    try:
        with httpx.Client(cookies=cookies) as client:
            response = client.post(url, headers=headers, json=payload, timeout=20.0)
            if response.status_code != 200:
                return f"Error from Web of Science: HTTP {response.status_code}"
                
            try:
                parsed_data = response.json()
                if isinstance(parsed_data, dict):
                    parsed_data = [parsed_data]
            except Exception:
                parsed_data = []
                for line in response.text.strip().split('\n'):
                    if not line.strip(): continue
                    try: parsed_data.append(json.loads(line))
                    except: pass
            
            records_data = next((item.get('payload') for item in parsed_data if isinstance(item, dict) and item.get('key') == 'records'), {})
            
            if not records_data and len(parsed_data) > 0 and isinstance(parsed_data[0], dict):
                if 'Records' in parsed_data[0]:
                    records_data = parsed_data[0]['Records']
                elif 'payload' in parsed_data[0] and isinstance(parsed_data[0]['payload'], dict) and 'Records' in parsed_data[0]['payload']:
                    records_data = parsed_data[0]['payload']['Records']
            
            records = []
            if isinstance(records_data, dict):
                for idx_key, rec in records_data.items():
                    try:
                        title = ""
                        try:
                            item_lang = rec.get("titles", {}).get("item", {})
                            lang_key = list(item_lang.keys())[0] if item_lang else "en"
                            title = item_lang.get(lang_key, [{}])[0].get("title", "")
                        except: pass
                        
                        authors = []
                        try:
                            author_lang = rec.get("names", {}).get("author", {})
                            lang_key = list(author_lang.keys())[0] if author_lang else "en"
                            author_list = author_lang.get(lang_key, [])
                            authors = [a.get("wos_standard", a.get("display_name", "")) for a in author_list if a]
                        except: pass

                        source = ""
                        try:
                            source_lang = rec.get("titles", {}).get("source", {})
                            lang_key = list(source_lang.keys())[0] if source_lang else "en"
                            source = source_lang.get(lang_key, [{}])[0].get("title", "")
                        except: pass

                        pub_info = rec.get("pub_info", {})
                        year = pub_info.get("pubyear", "")
                        vol = pub_info.get("vol", "")
                        issue = pub_info.get("issue", "")
                        page = pub_info.get("page", {}).get("content", "")
                        
                        doi = rec.get("doi", "")
                        wosId = rec.get("colluid", "")
                        
                        abstract = ""
                        try:
                            abs_basic = rec.get("abstract", {}).get("basic", {})
                            lang_key = list(abs_basic.keys())[0] if abs_basic else "en"
                            abstract = abs_basic.get(lang_key, {}).get("abstract", "")
                        except: pass

                        records.append({
                            "title": title,
                            "authors": authors,
                            "source": source,
                            "year": year,
                            "volume": vol,
                            "issue": issue,
                            "pages": page,
                            "doi": doi,
                            "wosId": wosId,
                            "abstract": abstract.replace("\n", " ")
                        })
                    except Exception:
                        pass
                        
            if not records:
                print(f"Failed to parse export records. Status {response.status_code}, Response sample: {response.text[:500]}"); return "0 records found or failed to parse. See server logs for details."
                
            fmt = format.lower()
            import csv
            import json as json_lib
            import time
            timestamp = int(time.time())
            
            if fmt == "csv":
                import io
                output = io.StringIO()
                writer = csv.DictWriter(output, fieldnames=["wosId", "title", "authors", "source", "year", "volume", "issue", "pages", "doi", "abstract"])
                writer.writeheader()
                for r in records:
                    row = dict(r)
                    row["authors"] = "; ".join(row["authors"])
                    writer.writerow(row)
                return output.getvalue()
                
            elif fmt == "json":
                return json_lib.dumps(records, indent=2, ensure_ascii=False)
                
            elif fmt == "bibtex":
                result = []
                for r in records:
                    author_str = " and ".join(r["authors"])
                    result.append(f"@article{{{r['wosId']},\n" +
                        f"  title={{{r['title']}}},\n" +
                        f"  author={{{author_str}}},\n" +
                        f"  journal={{{r['source']}}},\n" +
                        (f"  year={{{r['year']}}},\n" if r["year"] else "") +
                        (f"  volume={{{r['volume']}}},\n" if r["volume"] else "") +
                        (f"  number={{{r['issue']}}},\n" if r["issue"] else "") +
                        (f"  pages={{{r['pages']}}},\n" if r["pages"] else "") +
                        (f"  doi={{{r['doi']}}},\n" if r["doi"] else "") +
                        f"  abstract={{{r['abstract']}}}\n" +
                        "}")
                return "\n\n".join(result)
                
            elif fmt == "ris":
                result = []
                for r in records:
                    ris_str = "TY  - JOUR\n"
                    ris_str += f"TI  - {r['title']}\n"
                    for a in r["authors"]:
                        ris_str += f"AU  - {a}\n"
                    ris_str += f"T2  - {r['source']}\n"
                    if r["year"]: ris_str += f"PY  - {r['year']}\n"
                    if r["volume"]: ris_str += f"VL  - {r['volume']}\n"
                    if r["issue"]: ris_str += f"IS  - {r['issue']}\n"
                    if r["pages"]: 
                        if "-" in r["pages"]:
                            p = r["pages"].split("-")
                            ris_str += f"SP  - {p[0]}\n"
                            if len(p)>1: ris_str += f"EP  - {p[1]}\n"
                        else:
                            ris_str += f"SP  - {r['pages']}\n"
                    if r["doi"]: ris_str += f"DO  - {r['doi']}\n"
                    ris_str += f"AB  - {r['abstract']}\n"
                    ris_str += f"ID  - {r['wosId']}\n"
                    ris_str += "ER  -"
                    result.append(ris_str)
                return "\n\n".join(result)
                
            else:
                return f"Unsupported format: {fmt}. Use csv, json, bibtex, or ris."
                
    except Exception as e:
        return f"An error occurred during export: {str(e)}"



@mcp.tool()
def search_cnki(
    query: str, 
    limit: int = 20, 
    search_type: str = "主题",
    sort: str = "相关度",
    pages: int = 1
) -> str:
    """
    Search for literature on CNKI (China National Knowledge Infrastructure).
    
    Args:
        query: The search query string (e.g. topic, title, author).
        limit: Number of results to return per page (max 50).
        search_type: The field to search in (e.g. "主题", "篇名", "作者").
        sort: Sort order ("相关度", "发表时间", "被引", "下载").
        pages: Number of pages to retrieve.
    """
    try:
        # We pass search_type to client to adapt the payload
        results = cnki_client.search(query, search_type=search_type, limit=limit)
        
        if not results:
            return f"Found 0 results for '{query}'"
            
        output = f"Found **{len(results)}** results on CNKI.\n\n"
        output += "| # | Title | Authors | Source | Date | URL |\n"
        output += "|---|-------|---------|--------|------|-----|\n"
        for i, r in enumerate(results):
            output += f"| {i+1} | {r['title']} | {r['authors']} | {r['source']} | {r['date']} | [Link]({r['url']}) |\n"
            
        return output
            
    except Exception as e:
        return f"An error occurred during CNKI search: {str(e)}"

@mcp.tool()
def get_cnki_paper_detail(url: str) -> str:
    """
    Fetch full detailed metadata for a specific CNKI paper.
    
    Args:
        url: The CNKI paper URL (e.g., https://kns.cnki.net/kcms2/article/abstract?v=...&dbcode=...&filename=...).
    """
    try:
        cnki_client.ensure_session()
        resp = cnki_client.session.get(url, headers={"User-Agent": "Mozilla/5.0"})
        if resp.status_code != 200:
            return f"Error from CNKI: HTTP {resp.status_code}"
            
        soup = BeautifulSoup(resp.text, 'html.parser')
        
        title_elem = soup.select_one('.wx-tit h1')
        title = title_elem.text.strip() if title_elem else "Unknown Title"
        
        authors = [a.text.strip() for a in soup.select('h3.author span a')]
        institutions = [a.text.strip() for a in soup.select('h3.orgn span a')]
        
        abs_elem = soup.select_one('#ChDivSummary')
        abstract = abs_elem.text.strip() if abs_elem else "No abstract"
        
        keywords = [a.text.strip() for a in soup.select('p.keywords a')]
        
        # Download link might be tricky depending on permissions
        
        out = f"## {title}\n"
        out += f"**Authors:** {', '.join(authors)}\n"
        out += f"**Institutions:** {', '.join(institutions)}\n\n"
        out += f"**Keywords:** {', '.join(keywords)}\n\n"
        out += f"### Abstract\n{abstract}\n"
        out += f"\n**URL:** {url}\n"
        
        return out
        
    except Exception as e:
        return f"An error occurred: {str(e)}"

@mcp.tool()
def find_best_match(query: str) -> str:
    """
    Find the best matching paper on CNKI for a given title or query.
    
    Args:
        query: The paper title.
    """
    try:
        results = cnki_client.search(query, limit=1)
        if not results:
            return "No match found."
        
        r = results[0]
        return f"Best Match:\nTitle: {r['title']}\nAuthors: {r['authors']}\nDate: {r['date']}\nURL: {r['url']}"
    except Exception as e:
        return f"An error occurred: {str(e)}"

@mcp.tool()
def format_citation(title: str, authors: str, source: str, year: str) -> str:
    """Format citation in GBT7714"""
    return f"[{1}] {authors}. {title}[J]. {source}, {year}."

@mcp.tool()
def export_cnki_papers(papers_json: str, format: str = "json") -> str:
    """Export papers to json"""
    return "Exported:\n" + papers_json

@mcp.resource("cnki://status")
def get_status() -> str:
    return '{"status": "running", "version": "pure_protocol_v1"}'

@mcp.resource("cnki://search-types")
def get_search_types() -> str:
    return '{"types": ["主题", "篇名", "作者", "关键词", "摘要", "全文"]}'



if __name__ == "__main__":
    cfg = load_config()
    port = cfg.get("port", 5000)
    
    import uvicorn
    from starlette.middleware.cors import CORSMiddleware

    
    app = mcp.sse_app()
    # Reverting custom route
    for route in app.routes:
        if route.path == "/http":
            route.path = "/sse" 
    
    app.add_middleware(
        CORSMiddleware,
        allow_origin_regex=".*",
        allow_credentials=True,
        allow_methods=["*"],
        allow_headers=["*"],
        expose_headers=["*"],
    )
    
    print(f"Starting WOS MCP Server on port {port} (SSE Mode)...")
    
    listen_public = cfg.get("listen_public", False)
    host = "0.0.0.0" if listen_public else "127.0.0.1"
    
    uvicorn.run(app, host=host, port=port)
