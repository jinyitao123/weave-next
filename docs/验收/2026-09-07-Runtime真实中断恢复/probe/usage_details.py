"""Read complete JSON fields preceding a bounded, truncated raw CLI summary."""
import json


def complete_fields(raw):
    try:
        return json.loads(raw), True
    except (ValueError, TypeError):
        pass
    if not isinstance(raw, str) or not raw.lstrip().startswith('{'):
        return {}, False
    decoder = json.JSONDecoder()
    position = raw.index('{') + 1
    fields = {}
    while position < len(raw):
        while position < len(raw) and raw[position].isspace():
            position += 1
        try:
            key, position = decoder.raw_decode(raw, position)
            if not isinstance(key, str):
                break
            while position < len(raw) and raw[position].isspace():
                position += 1
            if raw[position] != ':':
                break
            position += 1
            while position < len(raw) and raw[position].isspace():
                position += 1
            value, position = decoder.raw_decode(raw, position)
            fields[key] = value
            while position < len(raw) and raw[position].isspace():
                position += 1
            if raw[position] != ',':
                break
            position += 1
        except (ValueError, IndexError):
            break
    return fields, False
