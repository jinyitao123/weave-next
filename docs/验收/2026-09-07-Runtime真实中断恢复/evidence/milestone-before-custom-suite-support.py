"""Conservative successful test-suite summary recognition for fault injection."""
import re

def successful_test_summary(output, truncated=False):
    if truncated or re.search(r'\b(?:FAIL|FAILED|FAILURES|ERROR|ERRORS)\b',output):
        return False
    unittest_summary = re.search(r'^Ran [1-9]\d* tests? in [^\n]+\r?$',output,re.M) and re.search(r'^OK\r?$',output,re.M)
    pytest_summary = re.search(r'^(?:=+\s*)?[1-9]\d* passed(?:, \d+ warnings?)? in [^\n]+(?:=+)?\r?$',output,re.M)
    return bool(unittest_summary or pytest_summary)
