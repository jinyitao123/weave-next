# ============================================================================
# corona_model/miniyaml.py — 极简 YAML 子集加载器（仅 Python 标准库）
# ----------------------------------------------------------------------------
# 目的：本机 Python 无 PyYAML，而 RQ-MDL-001 要求模型参数必须派生自
#       outputs/model/baseline_frozen.yaml。本加载器支持该基线文件实际使用的
#       YAML 子集：缩进嵌套映射、列表（含 "- key: value" 形式）、行内列表
#       [a, b]、标量（int/float/str/bool/null/带引号字符串）、# 注释、
#       折叠块标量（> / >-，内容按注释性文本跳过，不参与参数）。
# 边界：不是通用 YAML 解析器；对子集外语法抛出 ValueError 而非静默误解析。
# 追溯键: baseline_id=corona-baseline-1.0.0 baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# ============================================================================
"""Minimal YAML-subset loader (stdlib only) for baseline_frozen.yaml."""

from __future__ import annotations


class MiniYAMLError(ValueError):
    """Raised when input uses YAML syntax outside the supported subset."""


def _strip_comment(line: str) -> str:
    """Remove a trailing ``#`` comment that is not inside quotes."""
    in_single = in_double = False
    for i, ch in enumerate(line):
        if ch == "'" and not in_double:
            in_single = not in_single
        elif ch == '"' and not in_single:
            in_double = not in_double
        elif ch == "#" and not in_single and not in_double:
            if i == 0 or line[i - 1] in (" ", "\t"):
                return line[:i]
    return line


def _parse_scalar(token: str):
    token = token.strip()
    if token == "":
        return None
    if token.startswith("[") and token.endswith("]"):
        inner = token[1:-1].strip()
        if not inner:
            return []
        return [_parse_scalar(part) for part in inner.split(",")]
    if (token.startswith('"') and token.endswith('"')) or (
        token.startswith("'") and token.endswith("'")
    ):
        return token[1:-1]
    low = token.lower()
    if low in ("true", "false"):
        return low == "true"
    if low in ("null", "~"):
        return None
    try:
        return int(token)
    except ValueError:
        pass
    try:
        return float(token)
    except ValueError:
        pass
    return token


def _logical_lines(text: str):
    """Yield (indent, content) skipping blanks, comments and block scalars.

    Folded block scalars (``key: >`` / ``key: >-``) are annotation prose in the
    baseline file; their body lines are consumed and the key maps to the
    concatenated text so structure stays intact.
    """
    raw = text.splitlines()
    i = 0
    out = []
    while i < len(raw):
        line = raw[i]
        stripped = _strip_comment(line).rstrip()
        i += 1
        if not stripped.strip():
            continue
        indent = len(stripped) - len(stripped.lstrip(" "))
        content = stripped.strip()
        if content.endswith(": >") or content.endswith(": >-") or content.endswith(": |") or content.endswith(": |-"):
            key = content.split(":", 1)[0]
            body = []
            while i < len(raw):
                nxt = raw[i]
                if nxt.strip() == "":
                    i += 1
                    continue
                nindent = len(nxt) - len(nxt.lstrip(" "))
                if nindent <= indent:
                    break
                body.append(nxt.strip())
                i += 1
            out.append((indent, f"{key}: {' '.join(body)}"))
            continue
        if "\t" in stripped[: indent + 1]:
            raise MiniYAMLError(f"tab indentation not supported: {line!r}")
        out.append((indent, content))
    return out


def _parse_block(lines, pos, indent):
    """Parse mapping or list at given indent. Returns (obj, next_pos)."""
    if pos >= len(lines):
        return {}, pos
    first_indent, first_content = lines[pos]
    if first_indent < indent:
        return {}, pos
    is_list = first_content.startswith("- ") or first_content == "-"
    if is_list:
        result = []
        while pos < len(lines):
            ind, content = lines[pos]
            if ind != indent or not (content.startswith("- ") or content == "-"):
                break
            item_text = content[1:].strip()
            if item_text == "":
                # nested block belonging to this list item
                item, pos = _parse_block(lines, pos + 1, indent + 2)
                result.append(item)
            elif ":" in item_text and not item_text.startswith(('"', "'")):
                # "- key: value" starts a mapping item; following deeper
                # keys at indent+2 belong to the same item.
                key, _, val = item_text.partition(":")
                item = {}
                val = val.strip()
                if val == "":
                    sub, pos = _parse_block(lines, pos + 1, indent + 4)
                    item[key.strip()] = sub
                else:
                    item[key.strip()] = _parse_scalar(val)
                    pos += 1
                # absorb sibling keys of this mapping item (indent + 2)
                while pos < len(lines):
                    ind2, content2 = lines[pos]
                    if ind2 != indent + 2 or content2.startswith("- "):
                        break
                    if ":" not in content2:
                        raise MiniYAMLError(f"expected key: value, got {content2!r}")
                    k2, _, v2 = content2.partition(":")
                    v2 = v2.strip()
                    if v2 == "":
                        sub, pos = _parse_block(lines, pos + 1, indent + 4)
                        item[k2.strip()] = sub
                    else:
                        item[k2.strip()] = _parse_scalar(v2)
                        pos += 1
                result.append(item)
            else:
                result.append(_parse_scalar(item_text))
                pos += 1
        return result, pos
    # mapping
    result = {}
    while pos < len(lines):
        ind, content = lines[pos]
        if ind != indent or content.startswith("- "):
            break
        if ":" not in content:
            raise MiniYAMLError(f"expected key: value, got {content!r}")
        key, _, val = content.partition(":")
        key = key.strip()
        val = val.strip()
        if val == "":
            sub, pos = _parse_block(lines, pos + 1, indent + 2)
            result[key] = sub
        else:
            result[key] = _parse_scalar(val)
            pos += 1
    return result, pos


def loads(text: str):
    """Parse the supported YAML subset into Python dict/list/scalars."""
    lines = _logical_lines(text)
    if not lines:
        return {}
    obj, pos = _parse_block(lines, 0, lines[0][0])
    if pos != len(lines):
        raise MiniYAMLError(f"unparsed trailing content at line index {pos}")
    return obj


def load(path: str):
    with open(path, "r", encoding="utf-8") as fh:
        return loads(fh.read())
