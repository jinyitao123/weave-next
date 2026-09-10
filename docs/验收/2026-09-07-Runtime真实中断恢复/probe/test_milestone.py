import unittest
from milestone import successful_test_summary

class MilestoneTests(unittest.TestCase):
    def test_individual_ok_is_not_suite_success(self):
        self.assertFalse(successful_test_summary('test_one ... ok\ntest_two ... FAIL\nTraceback truncated'))
    def test_truncated_output_is_not_evidence(self):
        self.assertFalse(successful_test_summary('Ran 20 tests in 0.001s\n\nOK\n',True))
    def test_completed_unittest_summary_with_host_exit_suffix(self):
        self.assertTrue(successful_test_summary('test_one ... ok\nRan 20 tests in 0.001s\n\nOK\ntests exit=0\n'))
    def test_failed_suite_after_earlier_ok(self):
        self.assertFalse(successful_test_summary('Ran 20 tests in 0.001s\nOK\nRan 1 test in 0.1s\nFAILED (failures=1)'))
    def test_pytest_summary(self):
        self.assertTrue(successful_test_summary('================ 12 passed in 0.11s ================\n'))
    def test_empty_suite_not_valid(self):
        self.assertFalse(successful_test_summary('Ran 0 tests in 0.000s\nOK\n'))
    def test_custom_counted_summary(self):
        self.assertTrue(successful_test_summary('[summary] 18/18 checks passed -> PASS\nEXIT=0\n'))
    def test_custom_summary_requires_all_checks_and_exit(self):
        for text in ['[summary] 17/18 checks passed -> PASS\nEXIT=0\n', '[summary] 0/0 checks passed -> PASS\nEXIT=0\n', '[summary] 18/18 checks passed -> PASS\nEXIT=1\n', '[summary] 18/18 checks passed -> PASS\n']:
            self.assertFalse(successful_test_summary(text))
if __name__=='__main__':unittest.main()
