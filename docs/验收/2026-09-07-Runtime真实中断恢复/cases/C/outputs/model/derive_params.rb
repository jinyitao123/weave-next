#!/usr/bin/env ruby
# 派生参数: baseline_frozen.yaml -> parameters.json
# 追溯键: baseline_id=corona-baseline-1.0.0 · baseline_version=1.0.0
#         content_digest=cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2
# 规则: ICD-SOT-001/002 — baseline_frozen.yaml 为唯一参数源; 本脚本只做受控格式转换,
#       转换前核验 SHA-256 摘要, 不匹配即失败退出(非零), 不静默继续。
require 'yaml'
require 'json'
require 'digest'

EXPECTED_DIGEST = 'cfb12b781363c547da87e6dec9fd937079cba25501b85ff6cbfabfbc508b10c2'
src = File.join(__dir__, 'baseline_frozen.yaml')
dst = File.join(__dir__, 'parameters.json')

actual = Digest::SHA256.file(src).hexdigest
abort("FATAL: baseline digest mismatch: #{actual} != #{EXPECTED_DIGEST}") unless actual == EXPECTED_DIGEST

data = YAML.load_file(src)
params = {
  'traceability' => {
    'baseline_id' => data['baseline_id'],
    'baseline_version' => data['baseline_version'],
    'content_digest' => actual,
    'derived_from' => 'baseline_frozen.yaml',
    'derivation' => 'derive_params.rb (YAML->JSON 受控格式转换, 无数值改动)'
  },
  'scenarios' => { 'cruise_speed_c' => data['scenarios']['cruise_speed_c'], 'locked' => true },
  'constants' => data['constants'],
  'reference_cases' => data['reference_cases'],
  'reference_checks' => data['reference_checks'],
  'routes' => data['routes'],
  'truth_labels' => data['truth_labels'],
  'forbidden_claims' => data['forbidden_claims']
}
File.write(dst, JSON.pretty_generate(params) + "\n")
puts "OK: parameters.json derived, digest verified #{actual[0,8]}…#{actual[-4,4]}"
