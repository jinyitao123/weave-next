#!/usr/bin/env python3
"""
mini_yaml.py - 自包含的 YAML 子集解析器（仅覆盖冻结基线 baseline.yaml 的结构）。

不依赖第三方 PyYAML，使模型与测试在任何 Python 3.8+ 环境均可运行。
覆盖结构:
  顶层/嵌套标量        key: value
  井号注释             # ...
  嵌套映射             key:\n  subkey: value
  标量列表             key:\n  - item
  内联列表             key: [a, b, c]
  内联映射             key: {a: 1, b: 2}
  映射元素列表         - id: x\n    mass_kg: 1

值类型自动推断: int / float / str / bool / None / list / dict。
"""

import re


def _scalar(text):
    text = text.strip()
    if text == "":
        return None
    if text.startswith('"') and text.endswith('"'):
        return text[1:-1]
    if text.startswith("'") and text.endswith("'"):
        return text[1:-1]
    low = text.lower()
    if low in ("true", "yes"):
        return True
    if low in ("false", "no"):
        return False
    if low in ("null", "~"):
        return None
    if text.startswith("[") and text.endswith("]"):
        inner = text[1:-1].strip()
        if inner == "":
            return []
        return [_scalar(p) for p in _split_top(inner)]
    if text.startswith("{") and text.endswith("}"):
        inner = text[1:-1].strip()
        if inner == "":
            return {}
        out = {}
        for part in _split_top(inner):
            if ":" in part:
                k, v = part.split(":", 1)
                out[k.strip()] = _scalar(v)
        return out
    try:
        if re.fullmatch(r"[-+]?\d+", text):
            return int(text)
        if re.fullmatch(r"[-+]?(\d+\.\d*|\.\d+)([eE][-+]?\d+)?", text) or \
           re.fullmatch(r"[-+]?\d+[eE][-+]?\d+", text):
            return float(text)
    except ValueError:
        pass
    return text


def _split_top(text):
    """按顶层逗号切分，忽略方括号/花括号/引号内的逗号。"""
    parts = []
    depth = 0
    quote = None
    cur = []
    for ch in text:
        if quote:
            cur.append(ch)
            if ch == quote:
                quote = None
            continue
        if ch in "\"'":
            quote = ch
            cur.append(ch)
            continue
        if ch in "[{":
            depth += 1
            cur.append(ch)
            continue
        if ch in "]}":
            depth -= 1
            cur.append(ch)
            continue
        if ch == "," and depth == 0:
            parts.append("".join(cur).strip())
            cur = []
            continue
        cur.append(ch)
    parts.append("".join(cur).strip())
    return [p for p in parts if p]


def _parse_block(lines, i, indent):
    if lines[i].lstrip().startswith("-"):
        return _parse_list(lines, i, indent)
    return _parse_map(lines, i, indent)


def _parse_list(lines, i, indent):
    """解析以 '-' 开头、缩进为 indent 的列表。返回 (list, 下一索引)。"""
    items = []
    while i < len(lines):
        line = lines[i]
        stripped = line.strip()
        if stripped == "" or stripped.startswith("#"):
            i += 1
            continue
        cur_indent = len(line) - len(line.lstrip())
        if cur_indent < indent:
            break
        if cur_indent > indent:
            # 不应到达：属于上一条目的嵌套内容，已由 map 分支消化
            i += 1
            continue
        if not stripped.startswith("-"):
            break
        rest = stripped[1:].strip()
        if rest == "":
            i += 1
            continue
        # 映射元素: - key: value [, 后续缩进更深的行]
        if ":" in rest and not rest.startswith(("[", "{")):
            elem = {}
            first_key, _, first_val = rest.partition(":")
            elem[first_key.strip()] = _scalar(first_val)
            i += 1
            while i < len(lines):
                ln = lines[i]
                if ln.strip() == "" or ln.strip().startswith("#"):
                    i += 1
                    continue
                li = len(ln) - len(ln.lstrip())
                if li <= indent:
                    break
                if ln.lstrip().startswith("-"):
                    break
                if ":" in ln:
                    k, _, v = ln.partition(":")
                    elem[k.strip()] = _scalar(v)
                i += 1
            items.append(elem)
        else:
            items.append(_scalar(rest))
            i += 1
    return items, i


def _parse_map(lines, i, indent):
    data = {}
    while i < len(lines):
        line = lines[i]
        stripped = line.strip()
        if stripped == "" or stripped.startswith("#"):
            i += 1
            continue
        cur_indent = len(line) - len(line.lstrip())
        if cur_indent < indent:
            break
        if cur_indent > indent:
            i += 1
            continue
        if stripped.startswith("-"):
            break
        if ":" not in stripped:
            i += 1
            continue
        key, _, remainder = stripped.partition(":")
        key = key.strip()
        remainder = remainder.strip()
        if remainder == "" and i + 1 < len(lines):
            nxt = lines[i + 1]
            nxt_indent = len(nxt) - len(nxt.lstrip())
            if nxt_indent > indent:
                val, ni = _parse_block(lines, i + 1, nxt_indent)
                data[key] = val
                i = ni
                continue
        data[key] = _scalar(remainder)
        i += 1
    return data, i


def safe_load(text):
    """解析 YAML 子集文本，返回 Python 对象。"""
    lines = []
    for line in text.splitlines():
        if line.strip().startswith("#"):
            lines.append("")
        else:
            lines.append(line)
    i = 0
    while i < len(lines) and lines[i].strip() == "":
        i += 1
    if i >= len(lines):
        return {}
    indent = len(lines[i]) - len(lines[i].lstrip())
    value, _ = _parse_map(lines, i, indent)
    return value


if __name__ == "__main__":
    import sys
    p = sys.argv[1] if len(sys.argv) > 1 else "baseline.yaml"
    with open(p, "r", encoding="utf-8") as fh:
        doc = safe_load(fh.read())
    import pprint
    pprint.pprint(doc)
