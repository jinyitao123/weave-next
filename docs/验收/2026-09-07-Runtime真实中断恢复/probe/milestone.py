"""Conservative successful test-suite summary recognition for fault injection."""
import re

def successful_test_summary(output, truncated=False):
    if truncated or re.search(r'\b(?:FAIL|FAILED|FAILURES|ERROR|ERRORS)\b',output):
        return False
    unittest_summary = re.search(r'^Ran [1-9]\d* tests? in [^\n]+\r?$',output,re.M) and re.search(r'^OK\r?$',output,re.M)
    pytest_summary = re.search(r'^(?:=+\s*)?[1-9]\d* passed(?:, \d+ warnings?)? in [^\n]+(?:=+)?\r?$',output,re.M)
    # Counted custom model suites are also allowed by the registered criterion.
    # Require every counted check and the explicit process exit to succeed.
    custom_summary = re.search(r'^\[summary\] ([1-9]\d*)/\1 checks passed -> PASS\r?$',output,re.M) and re.search(r'^EXIT=0\r?$',output,re.M)
    return bool(unittest_summary or pytest_summary or custom_summary)
